# Build the reliability agent as a small static image.
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
# Ship the example SLO config; override by mounting your own at /slo.yaml.
COPY slo.yaml /slo.yaml
WORKDIR /
ENTRYPOINT ["/agent"]
CMD ["slo", "--config", "/slo.yaml"]
