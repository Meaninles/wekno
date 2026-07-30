package vlmguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

type Result struct {
	Content          string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	TTFT             time.Duration
	Duration         time.Duration
	LastProgressAge  time.Duration
	Streamed         bool
}

type streamOpenResult struct {
	stream *openai.ChatCompletionStream
	err    error
}

type streamEvent struct {
	response openai.ChatCompletionStreamResponse
	err      error
}

func Complete(
	ctx context.Context,
	client *openai.Client,
	request openai.ChatCompletionRequest,
	policy Policy,
) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return Result{}, errors.New("OpenAI VLM client is nil")
	}
	policy = normalizePolicy(policy)
	if !policy.Streaming {
		return completeWithoutStream(ctx, client, request, policy)
	}
	return completeWithStream(ctx, client, request, policy)
}

func completeWithStream(
	ctx context.Context,
	client *openai.Client,
	request openai.ChatCompletionRequest,
	policy Policy,
) (result Result, retErr error) {
	startedAt := time.Now()
	result.Streamed = true
	request.Stream = true
	request.StreamOptions = &openai.StreamOptions{IncludeUsage: true}

	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()

	totalTimer := time.NewTimer(policy.TotalTimeout)
	defer totalTimer.Stop()
	firstTokenTimer := time.NewTimer(policy.FirstTokenTimeout)
	defer firstTokenTimer.Stop()

	openResult := make(chan streamOpenResult, 1)
	go func() {
		stream, err := client.CreateChatCompletionStream(requestCtx, request)
		if requestCtx.Err() != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return
		}
		select {
		case openResult <- streamOpenResult{stream: stream, err: err}:
		case <-requestCtx.Done():
			if stream != nil {
				_ = stream.Close()
			}
		}
	}()

	var stream *openai.ChatCompletionStream
	select {
	case <-ctx.Done():
		result.Duration = time.Since(startedAt)
		return result, ctx.Err()
	case <-firstTokenTimer.C:
		cancelRequest()
		result.Duration = time.Since(startedAt)
		return result, newGuardError(
			FailureFirstTokenTimeout, policy, result, policy.FirstTokenTimeout, ctx.Err(),
		)
	case <-totalTimer.C:
		cancelRequest()
		result.Duration = time.Since(startedAt)
		return result, newGuardError(
			FailureFirstTokenTimeout, policy, result, policy.TotalTimeout, ctx.Err(),
		)
	case opened := <-openResult:
		if opened.err != nil {
			result.Duration = time.Since(startedAt)
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, fmt.Errorf("open OpenAI VLM stream: %w", opened.err)
		}
		stream = opened.stream
	}
	defer stream.Close()

	events := make(chan streamEvent, 1)
	go receiveStream(requestCtx, stream, events)

	var (
		contentBuilder  strings.Builder
		detector        repetitionDetector
		firstProgressAt time.Time
		lastProgressAt  time.Time
		idleTimer       = time.NewTimer(policy.IdleTimeout)
		idleTimerC      <-chan time.Time
	)
	if !idleTimer.Stop() {
		<-idleTimer.C
	}
	defer idleTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			cancelRequest()
			result = finishResult(result, contentBuilder.String(), startedAt, lastProgressAt)
			return result, ctx.Err()
		case <-firstTokenTimer.C:
			cancelRequest()
			_ = stream.Close()
			result = finishResult(result, contentBuilder.String(), startedAt, lastProgressAt)
			return result, newGuardError(
				FailureFirstTokenTimeout, policy, result, policy.FirstTokenTimeout, nil,
			)
		case <-idleTimerC:
			cancelRequest()
			_ = stream.Close()
			result = finishResult(result, contentBuilder.String(), startedAt, lastProgressAt)
			return result, newGuardError(
				FailureIdleTimeout, policy, result, policy.IdleTimeout, nil,
			)
		case <-totalTimer.C:
			cancelRequest()
			_ = stream.Close()
			result = finishResult(result, contentBuilder.String(), startedAt, lastProgressAt)
			kind := FailureTotalBudget
			if firstProgressAt.IsZero() {
				kind = FailureFirstTokenTimeout
			}
			return result, newGuardError(kind, policy, result, policy.TotalTimeout, nil)
		case event := <-events:
			if event.err != nil {
				result = finishResult(
					result, contentBuilder.String(), startedAt, lastProgressAt,
				)
				if errors.Is(event.err, io.EOF) {
					// go-openai reports both a protocol [DONE] marker and a
					// transport that closed early as io.EOF. A terminal
					// finish_reason proves the provider completed the choice;
					// without one, treating partial content as success would
					// silently persist a truncated OCR result.
					if result.FinishReason == "" {
						return result, newGuardError(
							FailureStreamTruncated,
							policy,
							result,
							0,
							io.ErrUnexpectedEOF,
						)
					}
					return result, nil
				}
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, fmt.Errorf("receive OpenAI VLM stream: %w", event.err)
			}

			if event.response.Usage != nil {
				result.PromptTokens = event.response.Usage.PromptTokens
				result.CompletionTokens = event.response.Usage.CompletionTokens
			}
			for _, choice := range event.response.Choices {
				progress := choice.Delta.Content != "" ||
					choice.Delta.ReasoningContent != ""
				if progress {
					now := time.Now()
					if firstProgressAt.IsZero() {
						firstProgressAt = now
						result.TTFT = now.Sub(startedAt)
						stopTimer(firstTokenTimer)
					}
					lastProgressAt = now
					resetTimer(idleTimer, policy.IdleTimeout)
					idleTimerC = idleTimer.C
				}
				if choice.Delta.Content != "" {
					contentBuilder.WriteString(choice.Delta.Content)
					if policy.DetectRunaway && detector.Observe(contentBuilder.String()) {
						cancelRequest()
						_ = stream.Close()
						result = finishResult(
							result, contentBuilder.String(), startedAt, lastProgressAt,
						)
						return result, newGuardError(
							FailureRunaway, policy, result, 0, nil,
						)
					}
				}
				if choice.FinishReason != "" &&
					choice.FinishReason != openai.FinishReasonNull {
					result.FinishReason = string(choice.FinishReason)
					// The provider has declared generation complete. Do not
					// turn a delayed usage/[DONE] trailer into an idle stall.
					stopTimer(idleTimer)
					idleTimerC = nil
					if isOutputLimitReason(result.FinishReason) {
						cancelRequest()
						_ = stream.Close()
						result = finishResult(
							result, contentBuilder.String(), startedAt, lastProgressAt,
						)
						return result, newGuardError(
							FailureOutputLimit, policy, result, 0, nil,
						)
					}
				}
			}
		}
	}
}

func completeWithoutStream(
	ctx context.Context,
	client *openai.Client,
	request openai.ChatCompletionRequest,
	policy Policy,
) (result Result, retErr error) {
	startedAt := time.Now()
	request.Stream = false
	request.StreamOptions = nil
	requestCtx, cancel := context.WithTimeout(ctx, policy.TotalTimeout)
	defer cancel()

	response, err := client.CreateChatCompletion(requestCtx, request)
	result.Duration = time.Since(startedAt)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return result, newGuardError(
				FailureFirstTokenTimeout, policy, result, policy.TotalTimeout, err,
			)
		}
		return result, fmt.Errorf("call OpenAI VLM: %w", err)
	}
	if len(response.Choices) == 0 {
		return result, errors.New("OpenAI VLM returned no choices")
	}

	result.Content = response.Choices[0].Message.Content
	result.FinishReason = string(response.Choices[0].FinishReason)
	result.PromptTokens = response.Usage.PromptTokens
	result.CompletionTokens = response.Usage.CompletionTokens
	result.LastProgressAge = 0

	if isOutputLimitReason(result.FinishReason) {
		return result, newGuardError(FailureOutputLimit, policy, result, 0, nil)
	}
	if policy.DetectRunaway {
		var detector repetitionDetector
		if detector.Observe(result.Content) {
			return result, newGuardError(FailureRunaway, policy, result, 0, nil)
		}
	}
	return result, nil
}

func receiveStream(
	ctx context.Context,
	stream *openai.ChatCompletionStream,
	events chan<- streamEvent,
) {
	for {
		response, err := stream.Recv()
		select {
		case events <- streamEvent{response: response, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func finishResult(
	result Result,
	content string,
	startedAt time.Time,
	lastProgressAt time.Time,
) Result {
	result.Content = content
	result.Duration = time.Since(startedAt)
	if !lastProgressAt.IsZero() {
		result.LastProgressAge = time.Since(lastProgressAt)
	}
	return result
}

func newGuardError(
	kind FailureKind,
	policy Policy,
	result Result,
	limit time.Duration,
	cause error,
) *Error {
	return &Error{
		Kind:          kind,
		Operation:     policy.Operation,
		Limit:         limit,
		Elapsed:       result.Duration,
		ProgressChars: utf8.RuneCountInString(result.Content),
		FinishReason:  result.FinishReason,
		Cause:         cause,
	}
}

func normalizePolicy(policy Policy) Policy {
	config := Config{
		Streaming:          policy.Streaming,
		FirstTokenTimeout:  policy.FirstTokenTimeout,
		IdleTimeout:        policy.IdleTimeout,
		TotalTimeout:       policy.TotalTimeout,
		GeneralMaxTokens:   policy.MaxTokens,
		OCRMaxTokens:       policy.MaxTokens,
		CaptionMaxTokens:   policy.MaxTokens,
		GeneralTemperature: policy.Temperature,
		OCRTemperature:     policy.Temperature,
		CaptionTemperature: policy.Temperature,
	}.normalized()
	normalized := config.Policy(policy.Operation)
	normalized.Streaming = policy.Streaming
	normalized.DetectRunaway = policy.DetectRunaway
	return normalized
}

func isOutputLimitReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_completion_tokens":
		return true
	default:
		return false
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	stopTimer(timer)
	timer.Reset(duration)
}
