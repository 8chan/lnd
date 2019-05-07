package lnwallet

import (
	"github.com/wakiyamap/monautil"
	"github.com/wakiyamap/monawallet/wallet/txrules"
	"github.com/wakiyamap/lnd/input"
)

// DefaultDustLimit is used to calculate the dust HTLC amount which will be
// send to other node during funding process.
func DefaultDustLimit() monautil.Amount {
	return txrules.GetDustThreshold(input.P2WSHSize, txrules.DefaultRelayFeePerKb)
}
