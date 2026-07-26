.PHONY: fmt test vet demo run run-demo-agent run-demo-agent-buggy watch-logs

fmt:
	gofmt -w cmd internal

test:
	go test ./...

vet:
	go vet ./...

demo:
	go test -v ./integration -run 'TestCompleteAuthenticatedAPIAndSLOFlow|TestCompleteAuditWatchFiringAndRecoveryFlow'

run:
	go run ./cmd/reliability-agent

run-demo-agent:
	go run ./cmd/demo-agent

run-demo-agent-buggy:
	go run ./cmd/demo-agent --buggy

watch-logs:
	go run ./cmd/reliability-agent audit-watch --profile examples/log-demo-backend.yaml
