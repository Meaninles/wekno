package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
)

type queueSnapshot struct {
	Queue          string `json:"queue"`
	Pending        int    `json:"pending"`
	Active         int    `json:"active"`
	Scheduled      int    `json:"scheduled"`
	Retry          int    `json:"retry"`
	Aggregating    int    `json:"aggregating"`
	Archived       int    `json:"archived"`
	Completed      int    `json:"completed"`
	ProcessedToday int    `json:"processed_today"`
	FailedToday    int    `json:"failed_today"`
	ProcessedTotal int    `json:"processed_total"`
	FailedTotal    int    `json:"failed_total"`
	Paused         bool   `json:"paused"`
	LatencyMS      int64  `json:"latency_ms"`
}

type auditReport struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Queues      []queueSnapshot `json:"queues"`
	Totals      queueSnapshot   `json:"totals"`
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", name)
		os.Exit(2)
	}
	return value
}

func main() {
	db := 0
	if rawDB := strings.TrimSpace(os.Getenv("REDIS_DB")); rawDB != "" {
		parsed, err := strconv.Atoi(rawDB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid REDIS_DB: %v\n", err)
			os.Exit(2)
		}
		db = parsed
	}
	inspector := asynq.NewInspector(asynq.RedisClientOpt{
		Addr:     requiredEnv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       db,
	})
	defer inspector.Close()

	queues, err := inspector.Queues()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list queues: %v\n", err)
		os.Exit(1)
	}
	sort.Strings(queues)
	report := auditReport{
		GeneratedAt: time.Now().UTC(),
		Queues:      make([]queueSnapshot, 0, len(queues)),
	}
	for _, queue := range queues {
		info, infoErr := inspector.GetQueueInfo(queue)
		if infoErr != nil {
			fmt.Fprintf(os.Stderr, "inspect queue %s: %v\n", queue, infoErr)
			os.Exit(1)
		}
		snapshot := queueSnapshot{
			Queue:          queue,
			Pending:        info.Pending,
			Active:         info.Active,
			Scheduled:      info.Scheduled,
			Retry:          info.Retry,
			Aggregating:    info.Aggregating,
			Archived:       info.Archived,
			Completed:      info.Completed,
			ProcessedToday: info.Processed,
			FailedToday:    info.Failed,
			ProcessedTotal: info.ProcessedTotal,
			FailedTotal:    info.FailedTotal,
			Paused:         info.Paused,
			LatencyMS:      info.Latency.Milliseconds(),
		}
		report.Queues = append(report.Queues, snapshot)
		report.Totals.Pending += snapshot.Pending
		report.Totals.Active += snapshot.Active
		report.Totals.Scheduled += snapshot.Scheduled
		report.Totals.Retry += snapshot.Retry
		report.Totals.Aggregating += snapshot.Aggregating
		report.Totals.Archived += snapshot.Archived
		report.Totals.Completed += snapshot.Completed
		report.Totals.ProcessedToday += snapshot.ProcessedToday
		report.Totals.FailedToday += snapshot.FailedToday
		report.Totals.ProcessedTotal += snapshot.ProcessedTotal
		report.Totals.FailedTotal += snapshot.FailedTotal
		if snapshot.Paused {
			report.Totals.Paused = true
		}
		if snapshot.LatencyMS > report.Totals.LatencyMS {
			report.Totals.LatencyMS = snapshot.LatencyMS
		}
	}
	report.Totals.Queue = "all"

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}
