FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /crickets ./cmd/crickets

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source=https://github.com/Infra-Dots/crickets
COPY --from=build /crickets /crickets
USER 65532:65532
ENTRYPOINT ["/crickets"]
