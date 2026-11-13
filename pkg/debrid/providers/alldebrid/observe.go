package alldebrid

import (
	"strconv"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func observe(statusCode int) types.Observation {
	o := types.Observation{Raw: strconv.Itoa(statusCode)}
	switch {
	case statusCode == 0:
		o.Phase = types.PhaseRemoteQueued
	case statusCode >= 1 && statusCode <= 3:
		o.Phase = types.PhaseDownloading
	case statusCode == allDebridReadyStatusCode:
		o.Phase = types.PhaseDownloaded
	case statusCode == allDebridNoPeerStatusCode:
		o.Phase = types.PhaseStalled
	case statusCode >= allDebridFirstFailedStatusCode:
		o.Phase = types.PhaseFailed
	default:
		o.Phase = types.PhaseUnknown
	}
	return o
}
