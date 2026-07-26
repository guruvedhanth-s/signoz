# One image, two binaries, selected with --build-arg SERVICE=api|payments.
#
# Version is stamped into the binary so the resource attribute and the deploy
# marker report something meaningful. Pass --build-arg VERSION=$(git rev-parse
# --short HEAD) to make "what changed?" answerable.
FROM golang:1.25-bookworm AS build

ARG SERVICE=api
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/guruvedhanth-s/signoz/demo-app/internal/telemetry.Version=${VERSION}" \
    -o /out/service ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot AS run

WORKDIR /app

COPY --from=build /out/service /app/service
# Only the api serves it, but copying unconditionally keeps one image.
COPY web/ /app/web/

EXPOSE 8090 8091

ENTRYPOINT ["/app/service"]
