FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /healthchecks-go ./cmd/healthchecks-go

FROM scratch
COPY LICENSE /LICENSE
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /healthchecks-go /healthchecks-go
WORKDIR /config
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/healthchecks-go"]
