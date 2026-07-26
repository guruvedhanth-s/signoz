# SRE Sidekick container image (PRD section 19). Builds the reliability-agent
# binary that implements every mode - server, slo, generate, audit-watch,
# diagnose, watch - so the same image serves both the alert-driven Slack
# loop (`watch`) and one-shot debug commands (`diagnose`, `slo`, ...).
#
# No custom SigNoz image: this is the sidekick's own third-layer service,
# built independently and joined to the Foundry-cast SigNoz network at
# runtime (`docker network connect`, see hackathon/DEMO.md), never a change
# to the SigNoz deployment itself (PRD non-goal 2).

FROM golang:1.25-bookworm AS build

WORKDIR /src

# Cached separately from the source copy so `go mod download` only reruns
# when go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/reliability-agent ./cmd/reliability-agent

FROM gcr.io/distroless/static-debian12:nonroot AS run

WORKDIR /app

COPY --from=build /out/reliability-agent /app/reliability-agent
COPY configs/sidekick.yaml /app/configs/sidekick.yaml
COPY examples/ /app/examples/

# watch's --webhook-listen default; diagnose/watch/audit-watch take
# everything else (SigNoz URL, credentials, MCP URL, ...) via flags or env
# at `docker run` time, so nothing else needs baking into the image.
EXPOSE 8082

ENTRYPOINT ["/app/reliability-agent"]
CMD ["watch"]
