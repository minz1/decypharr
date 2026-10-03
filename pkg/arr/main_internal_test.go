package arr

import (
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// testService registers one instance named "arr" and returns the service.
func testService(instance Arr) *Service {
	instance.Name = "arr"
	service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	service.arrs[instance.Name] = instance
	return service
}
