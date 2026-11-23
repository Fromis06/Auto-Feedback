.PHONY: init up down logs demo-feedback

init:
	[ -f .env ] || cp .env.example .env

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

demo-feedback:
	curl -fsS -X POST "http://localhost:$${ORCHESTRATOR_PORT:-8080}/api/v1/feedback" \
		-H "Content-Type: application/json" \
		-d '{"user_message":"Clicking Save on the Checkout page throws a 500 error and freezes the total calculation.","app_commit_sha":"deadbeef1234567890","breadcrumbs":[{"type":"navigation","path":"/cart","timestamp":"2026-11-18T03:00:00Z"},{"type":"click","target":"#checkout-save-btn","text":"Save","timestamp":"2026-11-18T03:00:05Z"}],"console_errors":[{"message":"TypeError: Cannot read properties of undefined (reading total)","source":"checkout.js:42"}],"network_logs":[{"method":"POST","url":"/api/checkout/save","status":500,"duration_ms":142}],"device_meta":{"user_agent":"Mozilla/5.0","viewport":"1440x900","platform":"Linux x86_64"}}'
