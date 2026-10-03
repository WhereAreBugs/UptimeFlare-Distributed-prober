FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG BUILD_TAGS=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags "$BUILD_TAGS" -trimpath -ldflags="-s -w" -o /light-prober ./cmd/light-prober
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /light-prober /light-prober
USER 65532:65532
VOLUME ["/data"]
ENTRYPOINT ["/light-prober", "--data-dir", "/data"]
