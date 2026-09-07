FROM weknora-dev-app:local AS checks
WORKDIR /workspace
COPY . .
ARG TEST_PATTERN=.
ARG TEST_PACKAGES="./internal/custom/modules/agentruntime ./internal/custom/modules/sourcerefs"
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    GOMODCACHE=/root/.cache/go-mod /usr/local/go/bin/go test -p 2 \
    -ldflags="-X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn" \
    -run "$TEST_PATTERN" -count=1 $TEST_PACKAGES
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    mkdir /out && for package in $TEST_PACKAGES; do \
      GOMODCACHE=/root/.cache/go-mod /usr/local/go/bin/go test -c -p 2 \
      -ldflags="-X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn" \
      -o "/out/$(basename "$package").test" "$package" || exit 1; \
    done
FROM scratch AS binaries
COPY --from=checks /out/ /
