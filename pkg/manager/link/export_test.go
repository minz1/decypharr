package link

import "time"

// SetFailureTTL lets tests expire remembered validation failures.
func SetFailureTTL(s *Service, ttl time.Duration) { s.failureTTL = ttl }
