FROM weknora-dev-app:local AS checks
WORKDIR /workspace
COPY . .
# Tests are injected only into this disposable test image. Application sources
# and the running eval services are not patched.
COPY custom/tests/agent_root_cause/dbanalytics_test.go.txt internal/custom/modules/dbanalytics/audit_rootcause_test.go

COPY custom/tests/agent_root_cause/retrieval_test.go.txt internal/agent/tools/audit_rootcause_test.go
ARG AUDIT_TEST_PATTERN=^TestAudit
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    mkdir -p /out && GOMODCACHE=/root/.cache/go-mod /usr/local/go/bin/go test -p 1 \
    -ldflags="-X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn" \
    -run "$AUDIT_TEST_PATTERN" -count=1 -v \
    ./internal/custom/modules/toolcontract \
    ./internal/custom/modules/sourcerefs \
    ./internal/custom/modules/conversationmemory \
    ./internal/custom/bootstrap \
    ./internal/application/service \
    ./internal/application/service/chat_pipeline \
    ./internal/handler/session \
    ./internal/custom/modules/dbanalytics \
    ./internal/custom/modules/generalagent \
    ./internal/agent/tools > /out/component-tests.log 2>&1 || (cat /out/component-tests.log && exit 1)
FROM scratch AS results
COPY --from=checks /out/ /
