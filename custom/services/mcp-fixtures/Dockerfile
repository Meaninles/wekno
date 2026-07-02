FROM golang:1.26-alpine AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY custom/services/mcp-fixtures ./custom/services/mcp-fixtures
RUN go build -o /out/weknora-mcp-fixtures ./custom/services/mcp-fixtures/cmd/server

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache tzdata
COPY --from=builder /out/weknora-mcp-fixtures /app/weknora-mcp-fixtures
EXPOSE 8092
ENTRYPOINT ["/app/weknora-mcp-fixtures"]
