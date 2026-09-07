package agentruntime

import (
	"os"
	"strings"
)

func artifactNotFound(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such file") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "nosuchkey") ||
		strings.Contains(message, "specified key does not exist")
}
