package push_proxy

import "errors"

var errInvalidGatewayConfig = errors.New("upstream and token verifier are required")
