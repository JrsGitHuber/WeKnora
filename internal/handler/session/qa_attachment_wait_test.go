package session

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestAgentAttachmentWaitTimeout(t *testing.T) {
	t.Setenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC", "90")
	assert.Equal(t, 90*time.Second, agentAttachmentWaitTimeout(nil))
	assert.Equal(t, 90*time.Second, agentAttachmentWaitTimeout(&types.CustomAgent{}))
	assert.Equal(t, 120*time.Second, agentAttachmentWaitTimeout(&types.CustomAgent{
		Config: types.CustomAgentConfig{AttachmentParseWaitTimeoutSec: 120},
	}))
	assert.Equal(t, 90*time.Second, agentAttachmentWaitTimeout(&types.CustomAgent{
		Config: types.CustomAgentConfig{AttachmentParseWaitTimeoutSec: -1},
	}), "-1 skips parsing at upload, the wait keeps the default")
}

func TestWaitForAttachmentsNonPositiveTimeoutSkipsWait(t *testing.T) {
	h := &Handler{}
	start := time.Now()
	h.waitForAttachments(context.Background(), 1, "session", []string{"doc"}, 0)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "must not poll when the wait is skipped")
}