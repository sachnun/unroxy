package core

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

// Public values embedded in every Psiphon client. They authenticate the
// signed server list served by the Psiphon Network and are not secret.
const (
	psiphonRemoteServerListSignaturePublicKey = "MIICIDANBgkqhkiG9w0BAQEFAAOCAg0AMIICCAKCAgEAt7Ls+/39r+T6zNW7GiVpJfzq/xvL9SBH5rIFnk0RXYEYavax3WS6HOD35eTAqn8AniOwiH+DOkvgSKF2caqk/y1dfq47Pdymtwzp9ikpB1C5OfAysXzBiwVJlCdajBKvBZDerV1cMvRzCKvKwRmvDmHgphQQ7WfXIGbRbmmk6opMBh3roE42KcotLFtqp0RRwLtcBRNtCdsrVsjiI1Lqz/lH+T61sGjSjQ3CHMuZYSQJZo/KrvzgQXpkaCTdbObxHqb6/+i1qaVOfEsvjoiyzTxJADvSytVtcTjijhPEV6XskJVHE1Zgl+7rATr/pDQkw6DPCNBS1+Y6fy7GstZALQXwEDN/qhQI9kWkHijT8ns+i1vGg00Mk/6J75arLhqcodWsdeG/M/moWgqQAnlZAGVtJI1OgeF5fsPpXu4kctOfuZlGjVZXQNW34aOzm8r8S0eVZitPlbhcPiR4gT/aSMz/wd8lZlzZYsje/Jr8u/YtlwjjreZrGRmG8KMOzukV3lLmMppXFMvl4bxv6YFEmIuTsOhbLTwFgh7KYNjodLj/LsqRVfwz31PgWQFTEPICV7GCvgVlPRxnofqKSjgTWI4mxDhBpVcATvaoBl1L/6WLbFvBsoAUBItWwctO2xalKxF5szhGm8lccoc5MZr8kfE0uxMgsxz4er68iCID+rsCAQM="
	psiphonServerEntrySignaturePublicKey      = "sHuUVTWaRyh5pZwy4UguSgkwmBe0EHtJJkoF5WrxmvA="
)

// The path segment "mjr4-p23r-puwl" is part of the Psiphon client config and
// has been unchanged since at least 2020. It is served from Amazon S3; the
// disposable domain-fronting mirrors Psiphon ships are intentionally omitted.
var psiphonRemoteServerListURLs = []string{
	"https://s3.amazonaws.com/psiphon/web/mjr4-p23r-puwl/server_list_compressed",
}

const serverEntryListCacheFile = "/tmp/unroxy-psiphon/server_entries.txt"

// loadServerEntries returns the latest signed server list. It always tries the
// network first and falls back to the last successful download on disk.
func loadServerEntries(ctx context.Context, logger *log.Logger) string {
	data, err := fetchRemoteServerList(ctx)
	if err == nil && strings.TrimSpace(data) != "" {
		logger.Printf("Psiphon: fetched %d server entries from remote list", countServerEntries(data))
		if mkErr := os.MkdirAll(filepath.Dir(serverEntryListCacheFile), 0755); mkErr == nil {
			_ = os.WriteFile(serverEntryListCacheFile, []byte(data), 0644)
		}
		return data
	}
	if err != nil {
		logger.Printf("Psiphon: remote server list fetch failed: %v", err)
	}
	cached, readErr := os.ReadFile(serverEntryListCacheFile)
	if readErr == nil && strings.TrimSpace(string(cached)) != "" {
		logger.Printf("Psiphon: using cached server entries")
		return string(cached)
	}
	return ""
}

func fetchRemoteServerList(ctx context.Context) (string, error) {
	var lastErr error
	for _, rawURL := range psiphonRemoteServerListURLs {
		data, err := fetchRemoteServerListURL(ctx, rawURL)
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return "", lastErr
}

func fetchRemoteServerListURL(ctx context.Context, rawURL string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", rawURL, resp.Status)
	}
	data, err := common.ReadAuthenticatedDataPackage(
		body, true, psiphonRemoteServerListSignaturePublicKey)
	if err != nil {
		return "", fmt.Errorf("%s: %w", rawURL, err)
	}
	return data, nil
}

func storeRemoteServerEntries(ctx context.Context, config *psiphon.Config, data string) error {
	return psiphon.StreamingStoreServerEntries(
		ctx,
		config,
		protocol.NewStreamingServerEntryDecoder(
			strings.NewReader(data),
			common.TruncateTimestampToHour(common.GetCurrentTimestamp()),
			protocol.SERVER_ENTRY_SOURCE_REMOTE),
		true)
}

func countServerEntries(data string) int {
	n := 0
	for _, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
