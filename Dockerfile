# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/scd-mock .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scd-mock /scd-mock
EXPOSE 4010 4080
HEALTHCHECK --interval=10s --timeout=3s --start-period=2s --retries=3 \
  CMD ["/scd-mock", "healthcheck"]
ENTRYPOINT ["/scd-mock"]
