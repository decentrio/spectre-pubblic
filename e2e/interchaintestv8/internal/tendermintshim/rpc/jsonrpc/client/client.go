package client

import (
	"net/http"

	cmtclient "github.com/cometbft/cometbft/rpc/jsonrpc/client"
)

func DefaultHTTPClient(remoteAddr string) (*http.Client, error) {
	return cmtclient.DefaultHTTPClient(remoteAddr)
}
