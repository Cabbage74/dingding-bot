APP_NAME := bot
BIN_DIR := ./bin
SERVER := server
REMOTE_DIR := /opt/dingding-bot

.PHONY: build deploy ssh clean run

build:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/bot

run:
	go run ./cmd/bot

clean:
	rm -rf $(BIN_DIR)

deploy: build
	scp $(BIN_DIR)/$(APP_NAME) $(SERVER):/tmp/
	ssh $(SERVER) "sudo mv /tmp/$(APP_NAME) $(REMOTE_DIR)/$(APP_NAME) && sudo systemctl restart bot"

deploy-config: build
	scp $(BIN_DIR)/$(APP_NAME) $(SERVER):/tmp/
	scp config.yaml $(SERVER):/tmp/
	ssh $(SERVER) "sudo mv /tmp/$(APP_NAME) $(REMOTE_DIR)/$(APP_NAME) && sudo mv /tmp/config.yaml $(REMOTE_DIR)/config.yaml && sudo systemctl restart bot"

ssh:
	ssh $(SERVER)
