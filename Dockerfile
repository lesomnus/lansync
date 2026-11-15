FROM golang:1.27 AS base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0


FROM base AS test

RUN --mount=type=cache,target=/root/.cache/go-build \
	go test -trimpath ./...


FROM --platform=$BUILDPLATFORM base AS builder

ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
	GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /lansync .


# distroless static: CA roots for controllers with a public certificate, and
# nothing else. Point -unifi-ca at the controller's own certificate otherwise.
FROM gcr.io/distroless/static-debian13:nonroot AS app

COPY --from=builder /lansync /lansync

ENTRYPOINT ["/lansync"]
CMD ["-h"]
