.PHONY: fmt test run run-demo-agent run-demo-agent-buggy watch-logs

fmt:
	gofmt -w cmd internal

test:
	go test ./...

run:
	go run ./cmd/reliability-agent

run-demo-agent:
	go run ./cmd/demo-agent

run-demo-agent-buggy:
	go run ./cmd/demo-agent --buggy

watch-logs:
	go run ./cmd/reliability-agent audit-watch --profile examples/demo-agent.yaml
