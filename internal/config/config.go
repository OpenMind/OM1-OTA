package config

import "os"

// GetEnv returns the value of the environment variable key, or def if unset or empty.
func GetEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
