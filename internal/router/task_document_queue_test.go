package router

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/types"
)

type fakeDocumentWorkflowRouter struct {
	forwarded  int
	processed  int
	forwardErr error
	processErr error
}

type fakeAsynqServerLifecycle struct {
	name      string
	events    *[]string
	startErr  error
	starts    int
	shutdowns int
}

func (f *fakeAsynqServerLifecycle) Start(asynq.Handler) error {
	f.starts++
	*f.events = append(*f.events, f.name+".start")
	return f.startErr
}

func (f *fakeAsynqServerLifecycle) Shutdown() {
	f.shutdowns++
	*f.events = append(*f.events, f.name+".shutdown")
}

type fakeDocumentQueueReadiness struct {
	events *[]string
	err    error
	calls  int
}

func (f *fakeDocumentQueueReadiness) MarkReady(context.Context) error {
	f.calls++
	*f.events = append(*f.events, "ready")
	return f.err
}

func (f *fakeDocumentWorkflowRouter) ForwardLegacyRoot(context.Context, *asynq.Task) error {
	f.forwarded++
	return f.forwardErr
}

func (f *fakeDocumentWorkflowRouter) Process(
	ctx context.Context,
	task *asynq.Task,
	delegate asynq.HandlerFunc,
) error {
	f.processed++
	if f.processErr != nil {
		return f.processErr
	}
	return delegate(ctx, task)
}

func TestLegacyDocumentRootsAreForwardedWithoutExecutingDelegate(t *testing.T) {
	for _, queue := range []string{types.QueueDefault, types.QueueDocumentHeavy} {
		t.Run(queue, func(t *testing.T) {
			coordinator := &fakeDocumentWorkflowRouter{}
			delegateCalls := 0
			task := asynq.NewTask(types.TypeDocumentProcess, []byte(`{"knowledge_id":"legacy"}`))
			err := routeDocumentRootTask(
				context.Background(), queue, true, task, coordinator,
				func(context.Context, *asynq.Task) error {
					delegateCalls++
					return nil
				},
			)
			if err != nil {
				t.Fatalf("route legacy root: %v", err)
			}
			if coordinator.forwarded != 1 || coordinator.processed != 0 || delegateCalls != 0 {
				t.Fatalf("legacy route counts: forwarded=%d processed=%d delegate=%d, want 1/0/0",
					coordinator.forwarded, coordinator.processed, delegateCalls)
			}
		})
	}
}

func TestDocumentQueueRootUsesCompleteWorkflowProcess(t *testing.T) {
	coordinator := &fakeDocumentWorkflowRouter{}
	delegateCalls := 0
	task := asynq.NewTask(types.TypeManualProcess, []byte(`{"knowledge_id":"document"}`))
	err := routeDocumentRootTask(
		context.Background(), types.QueueDocument, true, task, coordinator,
		func(context.Context, *asynq.Task) error {
			delegateCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("route document root: %v", err)
	}
	if coordinator.forwarded != 0 || coordinator.processed != 1 || delegateCalls != 1 {
		t.Fatalf("document route counts: forwarded=%d processed=%d delegate=%d, want 0/1/1",
			coordinator.forwarded, coordinator.processed, delegateCalls)
	}
}

func TestMissingQueueIdentityFailsClosedIntoLegacyForwarding(t *testing.T) {
	forwardErr := errors.New("redis unavailable")
	coordinator := &fakeDocumentWorkflowRouter{forwardErr: forwardErr}
	delegateCalls := 0
	err := routeDocumentRootTask(
		context.Background(), "", false,
		asynq.NewTask(types.TypeDocumentProcess, nil), coordinator,
		func(context.Context, *asynq.Task) error {
			delegateCalls++
			return nil
		},
	)
	if !errors.Is(err, forwardErr) {
		t.Fatalf("missing queue route error = %v, want forwarding error", err)
	}
	if coordinator.forwarded != 1 || coordinator.processed != 0 || delegateCalls != 0 {
		t.Fatalf("missing queue route executed wrong path: forwarded=%d processed=%d delegate=%d",
			coordinator.forwarded, coordinator.processed, delegateCalls)
	}
}

func TestDocumentOwnershipControlErrorsAreAcknowledgedBeforeDeadLetterMiddleware(t *testing.T) {
	for _, controlErr := range []error{
		documentqueue.ErrAlreadyLeased,
		documentqueue.ErrFairnessDeferred,
		documentqueue.ErrInstanceFenced,
		documentqueue.ErrLeaseLost,
		fmt.Errorf("wrapped control result: %w", documentqueue.ErrLeaseLost),
	} {
		if !isDocumentQueueControlError(controlErr) {
			t.Fatalf("control error was not classified for ACK: %v", controlErr)
		}
	}
	if isDocumentQueueControlError(errors.New("embedding failed")) {
		t.Fatal("business failure was incorrectly classified as control flow")
	}
	if isDocumentQueueControlError(documentqueue.ErrInstanceCapacity) {
		t.Fatal("capacity backpressure must reach Asynq retry handling instead of being ACKed")
	}
}

func TestDocumentServerConcurrencyComesFromCoordinatorCapacity(t *testing.T) {
	coordinator := documentqueue.NewCoordinatorWithConfig(
		nil, nil, "capacity-worker", "capacity-boot", 7, documentqueue.Config{},
	)
	if got := documentServerConcurrency(coordinator); got != 7 {
		t.Fatalf("document server concurrency = %d, want coordinator capacity 7", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("nil coordinator did not fail closed")
		}
	}()
	_ = documentServerConcurrency(nil)
}

func TestAsynqServersBecomeReadyOnlyAfterAllWorkersStart(t *testing.T) {
	events := []string{}
	normal := &fakeAsynqServerLifecycle{name: "normal", events: &events}
	multimodal := &fakeAsynqServerLifecycle{name: "multimodal", events: &events}
	wikiMap := &fakeAsynqServerLifecycle{name: "wiki-map", events: &events}
	control := &fakeAsynqServerLifecycle{name: "control", events: &events}
	document := &fakeAsynqServerLifecycle{name: "document", events: &events}
	part := &fakeAsynqServerLifecycle{name: "part", events: &events}
	readiness := &fakeDocumentQueueReadiness{events: &events}
	err := startAsynqServers(
		normal, multimodal, wikiMap, control, document, part,
		asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }), readiness,
	)
	if err != nil {
		t.Fatalf("start servers: %v", err)
	}
	want := []string{"normal.start", "multimodal.start", "wiki-map.start", "control.start", "document.start", "part.start", "ready"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("startup events = %v, want %v", events, want)
	}
	if normal.shutdowns != 0 || multimodal.shutdowns != 0 || wikiMap.shutdowns != 0 || control.shutdowns != 0 || document.shutdowns != 0 ||
		part.shutdowns != 0 || readiness.calls != 1 {
		t.Fatalf("success lifecycle counts: normal shutdown=%d multimodal shutdown=%d wiki-map shutdown=%d control shutdown=%d document shutdown=%d part shutdown=%d ready=%d",
			normal.shutdowns, multimodal.shutdowns, wikiMap.shutdowns, control.shutdowns, document.shutdowns, part.shutdowns, readiness.calls)
	}
}

func TestAsynqServerStartupFailureNeverMarksReadyAndRollsBack(t *testing.T) {
	tests := []struct {
		name           string
		normalErr      error
		multimodalErr  error
		wikiMapErr     error
		controlErr     error
		documentErr    error
		partErr        error
		readyErr       error
		wantEvents     []string
		wantNormal     int
		wantMultimodal int
		wantWikiMap    int
		wantControl    int
		wantDocument   int
		wantPart       int
	}{
		{
			name: "background fails", normalErr: errors.New("normal failed"),
			wantEvents: []string{"normal.start"},
		},
		{
			name: "multimodal fails", multimodalErr: errors.New("multimodal failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "normal.shutdown"},
			wantNormal: 1,
		},
		{
			name: "Wiki Map fails", wikiMapErr: errors.New("Wiki Map failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "wiki-map.start", "multimodal.shutdown", "normal.shutdown"},
			wantNormal: 1, wantMultimodal: 1,
		},
		{
			name: "control fails", controlErr: errors.New("control failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "wiki-map.start", "control.start", "wiki-map.shutdown", "multimodal.shutdown", "normal.shutdown"},
			wantNormal: 1, wantMultimodal: 1, wantWikiMap: 1,
		},
		{
			name: "document fails", documentErr: errors.New("document failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "wiki-map.start", "control.start", "document.start", "control.shutdown", "wiki-map.shutdown", "multimodal.shutdown", "normal.shutdown"},
			wantNormal: 1, wantMultimodal: 1, wantWikiMap: 1, wantControl: 1,
		},
		{
			name: "part fails", partErr: errors.New("part failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "wiki-map.start", "control.start", "document.start", "part.start", "document.shutdown", "control.shutdown", "wiki-map.shutdown", "multimodal.shutdown", "normal.shutdown"},
			wantNormal: 1, wantMultimodal: 1, wantWikiMap: 1, wantControl: 1, wantDocument: 1,
		},
		{
			name: "ready persistence fails", readyErr: errors.New("database failed"),
			wantEvents: []string{"normal.start", "multimodal.start", "wiki-map.start", "control.start", "document.start", "part.start", "ready", "part.shutdown", "document.shutdown", "control.shutdown", "wiki-map.shutdown", "multimodal.shutdown", "normal.shutdown"},
			wantNormal: 1, wantMultimodal: 1, wantWikiMap: 1, wantControl: 1, wantDocument: 1, wantPart: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			normal := &fakeAsynqServerLifecycle{name: "normal", events: &events, startErr: test.normalErr}
			multimodal := &fakeAsynqServerLifecycle{name: "multimodal", events: &events, startErr: test.multimodalErr}
			wikiMap := &fakeAsynqServerLifecycle{name: "wiki-map", events: &events, startErr: test.wikiMapErr}
			control := &fakeAsynqServerLifecycle{name: "control", events: &events, startErr: test.controlErr}
			document := &fakeAsynqServerLifecycle{name: "document", events: &events, startErr: test.documentErr}
			part := &fakeAsynqServerLifecycle{name: "part", events: &events, startErr: test.partErr}
			readiness := &fakeDocumentQueueReadiness{events: &events, err: test.readyErr}
			err := startAsynqServers(
				normal, multimodal, wikiMap, control, document, part,
				asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }),
				readiness,
			)
			if err == nil {
				t.Fatal("startup failure returned nil")
			}
			if !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("failure events = %v, want %v", events, test.wantEvents)
			}
			if normal.shutdowns != test.wantNormal || multimodal.shutdowns != test.wantMultimodal ||
				wikiMap.shutdowns != test.wantWikiMap || control.shutdowns != test.wantControl ||
				document.shutdowns != test.wantDocument ||
				part.shutdowns != test.wantPart {
				t.Fatalf("shutdowns normal/multimodal/wiki-map/control/document/part = %d/%d/%d/%d/%d/%d, want %d/%d/%d/%d/%d/%d",
					normal.shutdowns, multimodal.shutdowns, wikiMap.shutdowns, control.shutdowns, document.shutdowns, part.shutdowns,
					test.wantNormal, test.wantMultimodal, test.wantWikiMap, test.wantControl, test.wantDocument, test.wantPart)
			}
			wantReadyCalls := 0
			if test.readyErr != nil {
				wantReadyCalls = 1
			}
			if readiness.calls != wantReadyCalls {
				t.Fatalf("ready calls = %d, want %d", readiness.calls, wantReadyCalls)
			}
		})
	}
}
