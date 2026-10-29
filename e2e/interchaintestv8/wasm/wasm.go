package wasm

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	basePath                   = "e2e/interchaintestv8/wasm/"
	dummyLightClientFileName   = "cw_dummy_light_client.wasm.gz"
	wasmEthLightClientFileName = "cw_ics08_wasm_eth.wasm.gz"
	wasmArbitrumClientFileName = "cw_ics08_wasm_arbitrum.wasm.gz"
	wasmBaseClientFileName     = "cw_ics08_wasm_base.wasm.gz"
	wasmOpClientFileName       = "cw_ics08_wasm_op.wasm.gz"
)

func GetWasmDummyLightClient() (*os.File, error) {
	return os.Open(basePath + dummyLightClientFileName)
}

func GetLocalWasmEthLightClient() (*os.File, error) {
	return os.Open(basePath + wasmEthLightClientFileName)
}

// GetLocalWasmArbitrumClient opens the locally optimized Arbitrum client artifact.
func GetLocalWasmArbitrumClient() (*os.File, error) {
	return openLocalL2Artifact(wasmArbitrumClientFileName, "cw_ics08_wasm_arbitrum.wasm")
}

// GetLocalWasmBaseClient opens the locally optimized Base client artifact.
func GetLocalWasmBaseClient() (*os.File, error) {
	return openLocalL2Artifact(wasmBaseClientFileName, "cw_ics08_wasm_base.wasm")
}

// GetLocalWasmOpClient opens the locally optimized OP client artifact.
func GetLocalWasmOpClient() (*os.File, error) {
	return openLocalL2Artifact(wasmOpClientFileName, "cw_ics08_wasm_op.wasm")
}

// openLocalL2Artifact keeps compatibility with an explicitly generated gzip but
// otherwise uses the exact optimized artifact produced by the release validator.
// This removes stale byte blobs from deployment helpers while retaining stable
// artifact filenames.
func openLocalL2Artifact(gzipName, artifactName string) (*os.File, error) {
	candidates := []string{
		basePath + gzipName,
		"artifacts/" + artifactName,
		"../../../artifacts/" + artifactName,
	}
	var lastErr error
	for _, candidate := range candidates {
		file, err := os.Open(candidate)
		if err == nil {
			return file, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("open optimized %s from %v: %w", artifactName, candidates, lastErr)
}

func DownloadWasmEthLightClientRelease(release Release) (*os.File, error) {
	downloadUrl := fmt.Sprintf("%s/%s", release.BaseDownloadURL(), wasmEthLightClientFileName)

	resp, err := http.Get(downloadUrl) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	downloadFile, err := os.CreateTemp("", "eth-light-client-download")
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(downloadFile, resp.Body)
	if err != nil {
		downloadFile.Close()
		os.Remove(downloadFile.Name())
		return nil, err
	}

	// Seek to the beginning of the file before returning
	_, err = downloadFile.Seek(0, io.SeekStart)
	if err != nil {
		downloadFile.Close()
		os.Remove(downloadFile.Name())
		return nil, err
	}

	return downloadFile, nil
}
