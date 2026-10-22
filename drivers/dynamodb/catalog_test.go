package dynamodb

import (
	"testing"
)

func TestInvalidateDropsKeyMetadata(t *testing.T) {
	cc := newCatalogCache(&conn{})
	cc.putMeta(&tableMeta{name: "t", keys: []string{"id"}})
	cc.invalidate()
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if len(cc.metas) != 0 {
		t.Errorf("Invalidate kept key metadata: %v", cc.metas)
	}
}
