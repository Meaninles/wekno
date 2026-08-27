ARG RUNTIME_BASE_IMAGE=weknora-dev-app:local
FROM ${RUNTIME_BASE_IMAGE} AS builder

WORKDIR /workspace
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    GOMODCACHE=/root/.cache/go-mod /usr/local/go/bin/go build \
    -ldflags="-X 'google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn'" \
    -o /out/weknora-runtime-e2e ./cmd/server && \
    mkdir -p /out/go-mod/github.com/yanyiwu && \
    cp -a /root/.cache/go-mod/github.com/yanyiwu/. /out/go-mod/github.com/yanyiwu/

FROM ${RUNTIME_BASE_IMAGE}
COPY --from=builder /out/weknora-runtime-e2e /usr/local/bin/weknora-runtime-e2e
# gojieba resolves its dictionaries relative to the compile-time GOMODCACHE.
# Keep that exact path in the runtime image while still sourcing the files from
# the BuildKit cache through the durable /out copy.
COPY --from=builder /out/go-mod/github.com/yanyiwu/ /root/.cache/go-mod/github.com/yanyiwu/
WORKDIR /workspace
CMD ["/usr/local/bin/weknora-runtime-e2e"]
