package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// verifyPayment is a package-level variable allowing tests to stub the payment verification logic.
var verifyPayment = VerifyPayment

// VerifyPayment checks a tx hash against the chain via Tendermint RPC.
// Returns the actual amount paid (in uscrt) and the sender address.
func VerifyPayment(rpcURL, txHash, operatorAddr string, minAmount int64) (actualAmount int64, sender string, err error) {
	// Normalize the tx hash
	txHash = strings.TrimPrefix(txHash, "0x")
	txHash = strings.TrimPrefix(txHash, "0X")
	txHash = strings.ToUpper(txHash)

	// Query Tendermint RPC
	url := fmt.Sprintf("%s/tx?hash=0x%s", strings.TrimRight(rpcURL, "/"), txHash)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", fmt.Errorf("RPC request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("failed to read RPC response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("RPC returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse JSON response
	var rpcResp txRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return 0, "", fmt.Errorf("failed to parse RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return 0, "", fmt.Errorf("RPC error: %s", rpcResp.Error.Data)
	}

	if rpcResp.Result == nil {
		return 0, "", fmt.Errorf("tx not found: %s", txHash)
	}

	// Check tx success
	if rpcResp.Result.TxResult.Code != 0 {
		return 0, "", fmt.Errorf("tx failed with code %d: %s", rpcResp.Result.TxResult.Code, rpcResp.Result.TxResult.Log)
	}

	// Parse events to find transfer to operator
	foundSender, foundAmount, err := parseTransferEvents(rpcResp.Result.TxResult.Events, operatorAddr)
	if err != nil {
		return 0, "", err
	}

	if foundAmount < minAmount {
		return foundAmount, foundSender, fmt.Errorf("payment amount %d uscrt is less than minimum %d uscrt", foundAmount, minAmount)
	}

	return foundAmount, foundSender, nil
}

// parseTransferEvents looks through tx events for a transfer to the operator address
func parseTransferEvents(events []txEvent, operatorAddr string) (sender string, amount int64, err error) {
	for _, ev := range events {
		if ev.Type != "transfer" {
			continue
		}

		var currentRecipient, currentSender, currentAmount string

		for _, attr := range ev.Attributes {
			// Tendermint v0.34 RPC encodes event keys/values in base64.
			// Newer CometBFT versions return them as plain text. 
			// We check if the decoded bytes are valid UTF-8 to prevent accidentally decoding plaintext that looks like base64 (e.g. "1000000uscrt").
			keyBytes, err := base64.StdEncoding.DecodeString(attr.Key)
			key := attr.Key
			if err == nil && utf8.Valid(keyBytes) {
				key = string(keyBytes)
			}

			valBytes, err := base64.StdEncoding.DecodeString(attr.Value)
			val := attr.Value
			if err == nil && utf8.Valid(valBytes) {
				val = string(valBytes)
			}

			switch key {
			case "recipient":
				currentRecipient = val
			case "sender":
				currentSender = val
			case "amount":
				currentAmount = val
			}

			// Evaluate as soon as all three fields for a transfer tuple are populated.
			// This makes the logic order-independent (handles recipient->sender->amount, sender->amount->recipient, etc.)
			if currentRecipient != "" && currentSender != "" && currentAmount != "" {
				if currentRecipient == operatorAddr {
					parsed := parseUscrtAmount(currentAmount)
					if parsed > 0 {
						return currentSender, parsed, nil
					}
				}

				// Reset for the next transfer tuple in the same event
				currentRecipient = ""
				currentSender = ""
				currentAmount = ""
			}
		}
	}

	return "", 0, fmt.Errorf("no transfer event found to operator %s", operatorAddr)
}

// parseUscrtAmount parses amounts like "1000000uscrt" into int64
func parseUscrtAmount(s string) int64 {
	s = strings.TrimSuffix(s, "uscrt")
	var amount int64
	fmt.Sscanf(s, "%d", &amount)
	return amount
}

// JSON structures for Tendermint RPC /tx response
type txRPCResponse struct {
	Result *txResult `json:"result"`
	Error  *rpcError `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

type txResult struct {
	Hash     string       `json:"hash"`
	TxResult txResultData `json:"tx_result"`
}

type txResultData struct {
	Code   int       `json:"code"`
	Log    string    `json:"log"`
	Events []txEvent `json:"events"`
}

type txEvent struct {
	Type       string        `json:"type"`
	Attributes []txAttribute `json:"attributes"`
}

type txAttribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
