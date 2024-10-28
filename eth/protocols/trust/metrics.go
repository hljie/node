package trust

import (
	metrics "bsc-node/metrics"
)

var (
	ingressRegistrationErrorName = "eth/protocols/trust/ingress/registration/error"
	egressRegistrationErrorName  = "eth/protocols/trust/egress/registration/error"

	IngressRegistrationErrorMeter = metrics.NewRegisteredMeter(ingressRegistrationErrorName, nil)
	EgressRegistrationErrorMeter  = metrics.NewRegisteredMeter(egressRegistrationErrorName, nil)
)
