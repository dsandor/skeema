package tengo

import (
	"github.com/stretchr/testify/assert"
	"os"
	"testing"
)

func TestIntegrationSnowflakeConnect(t *testing.T) {
	dsn := os.Getenv("SNOWFLAKE_DSN")

	instance, err := NewInstance("snowflake", dsn)

	assert.NoError(t, err, "NewInstance should not return an error")
	assert.NotNil(t, instance, "instance should not be nil")

	conn, err := instance.Connect("CORE", "")

	assert.NoError(t, err, "Connect should not return an error")
	assert.NotNil(t, conn, "connection should not be nil")

}
