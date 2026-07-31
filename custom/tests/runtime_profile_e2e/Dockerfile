FROM weknora-dev-app:local AS builder

WORKDIR /workspace
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    /usr/local/go/bin/go build \
    -ldflags="-X 'google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn'" \
    -o /out/weknora-runtime-e2e ./cmd/server

FROM weknora-dev-app:local
COPY --from=builder /out/weknora-runtime-e2e /usr/local/bin/weknora-runtime-e2e
COPY --from=builder /go/pkg/mod/github.com/yanyiwu/ /go/pkg/mod/github.com/yanyiwu/
WORKDIR /workspace
CMD ["/usr/local/bin/weknora-runtime-e2e"]
