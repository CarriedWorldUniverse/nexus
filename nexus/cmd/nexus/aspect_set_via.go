// Broker-mediated aspect provider/model updates.

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type aspectProviderBindingResponse struct {
	Aspect   string `json:"aspect"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func runAspectSetViaBroker(name, brokerURL, adminToken string, provider, model *string) int {
	endpoint := strings.TrimRight(brokerURL, "/") + "/api/admin/aspects/" + url.PathEscape(name) + "/provider-binding"
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: os.Getenv("NEXUS_INSECURE_TLS") == "1",
			},
		},
	}

	current, ok := getAspectBindingFromBroker(client, endpoint, adminToken)
	if !ok {
		return 1
	}
	nextProvider := current.Provider
	nextModel := current.Model
	if provider != nil {
		nextProvider = *provider
	}
	if model != nil {
		nextModel = *model
	}

	reqBody := map[string]string{
		"provider": nextProvider,
		"model":    nextModel,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: marshal request: %v\n", err)
		return 1
	}
	httpReq, err := http.NewRequest(http.MethodPut, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: build request: %v\n", err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: PUT %s: %v\n", endpoint, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "aspect set: broker returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return 1
	}

	var updated aspectProviderBindingResponse
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: decode broker response: %v\n", err)
		return 1
	}
	fmt.Printf("aspect: %s\n", updated.Aspect)
	fmt.Printf("provider: %s -> %s\n", current.Provider, updated.Provider)
	fmt.Printf("model: %s -> %s\n", current.Model, updated.Model)
	fmt.Printf("via broker: %s\n", brokerURL)
	return 0
}

func getAspectBindingFromBroker(client *http.Client, endpoint, adminToken string) (aspectProviderBindingResponse, bool) {
	httpReq, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: build readback request: %v\n", err)
		return aspectProviderBindingResponse{}, false
	}
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: GET %s: %v\n", endpoint, err)
		return aspectProviderBindingResponse{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "aspect set: broker returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return aspectProviderBindingResponse{}, false
	}
	var current aspectProviderBindingResponse
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		fmt.Fprintf(os.Stderr, "aspect set: decode broker response: %v\n", err)
		return aspectProviderBindingResponse{}, false
	}
	return current, true
}
