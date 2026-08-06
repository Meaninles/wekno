package derivativequeue

import (
	"sort"
	"sync"
	"time"
)

// fairSelector is a process-local cursor over PostgreSQL candidates. The
// database remains authoritative; these cursors only choose the next bounded
// batch. Selection rotates pool -> tenant -> knowledge base, while aging is
// folded into ordering inside each KB so old low-priority work eventually
// overtakes newer work.
type fairSelector struct {
	mu           sync.Mutex
	poolCursor   int
	tenantCursor map[string]int
	kbCursor     map[string]int
}

type fairBucket struct {
	items []WorkItem
}

func (s *fairSelector) selectCandidates(
	input []WorkItem,
	limit int,
	now time.Time,
) []WorkItem {
	if limit <= 0 || len(input) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenantCursor == nil {
		s.tenantCursor = make(map[string]int)
		s.kbCursor = make(map[string]int)
	}

	// pool -> tenant -> KB -> ordered work
	groups := make(map[string]map[uint64]map[string]*fairBucket)
	for _, item := range input {
		pool := item.ResourcePoolID
		if pool == "" {
			pool = "unassigned"
		}
		if groups[pool] == nil {
			groups[pool] = make(map[uint64]map[string]*fairBucket)
		}
		if groups[pool][item.TenantID] == nil {
			groups[pool][item.TenantID] = make(map[string]*fairBucket)
		}
		if groups[pool][item.TenantID][item.KnowledgeBaseID] == nil {
			groups[pool][item.TenantID][item.KnowledgeBaseID] = &fairBucket{}
		}
		bucket := groups[pool][item.TenantID][item.KnowledgeBaseID]
		bucket.items = append(bucket.items, item)
	}
	pools := sortedStringKeys(groups)
	for _, tenants := range groups {
		for _, kbs := range tenants {
			for _, bucket := range kbs {
				sort.SliceStable(bucket.items, func(i, j int) bool {
					left := effectivePriority(bucket.items[i], now)
					right := effectivePriority(bucket.items[j], now)
					if left != right {
						return left > right
					}
					if !bucket.items[i].NextAttemptAt.Equal(bucket.items[j].NextAttemptAt) {
						return bucket.items[i].NextAttemptAt.Before(bucket.items[j].NextAttemptAt)
					}
					return bucket.items[i].CreatedAt.Before(bucket.items[j].CreatedAt)
				})
			}
		}
	}

	selected := make([]WorkItem, 0, min(limit, len(input)))
	for len(selected) < limit && len(selected) < len(input) {
		progress := false
		for poolOffset := 0; poolOffset < len(pools) && len(selected) < limit; poolOffset++ {
			pickedPool := false
			poolIndex := (s.poolCursor + poolOffset) % len(pools)
			pool := pools[poolIndex]
			tenants := sortedUintKeys(groups[pool])
			if len(tenants) == 0 {
				continue
			}
			tenantStart := s.tenantCursor[pool] % len(tenants)
			for tenantOffset := 0; tenantOffset < len(tenants); tenantOffset++ {
				tenant := tenants[(tenantStart+tenantOffset)%len(tenants)]
				kbs := sortedStringKeys(groups[pool][tenant])
				if len(kbs) == 0 {
					continue
				}
				cursorKey := pool + ":" + uintString(tenant)
				kbStart := s.kbCursor[cursorKey] % len(kbs)
				for kbOffset := 0; kbOffset < len(kbs); kbOffset++ {
					kbIndex := (kbStart + kbOffset) % len(kbs)
					bucket := groups[pool][tenant][kbs[kbIndex]]
					if len(bucket.items) == 0 {
						continue
					}
					selected = append(selected, bucket.items[0])
					bucket.items = bucket.items[1:]
					s.kbCursor[cursorKey] = (kbIndex + 1) % len(kbs)
					s.tenantCursor[pool] = (tenantStart + tenantOffset + 1) % len(tenants)
					progress = true
					pickedPool = true
					break
				}
				if pickedPool && len(selected) < limit {
					// Give the next pool a turn before returning to this tenant.
					break
				}
			}
		}
		if !progress {
			break
		}
		s.poolCursor = (s.poolCursor + 1) % len(pools)
	}
	return selected
}

func effectivePriority(item WorkItem, now time.Time) int64 {
	age := now.Sub(item.CreatedAt)
	if age < 0 {
		age = 0
	}
	// One priority point every 30 seconds, capped to avoid integer growth.
	aging := age / (30 * time.Second)
	if aging > 1000 {
		aging = 1000
	}
	return int64(item.Priority) + int64(aging)
}

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedUintKeys[V any](values map[uint64]V) []uint64 {
	keys := make([]uint64, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func uintString(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
