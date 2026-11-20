package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Tekrarlayan job alarmlari (or. InventoryStockJobErrorAlert): job her dakika calisir; tek tuk hata
// onemsizdir, ama ayni tenant (gonderen adres) icin hata STREAK_MIN boyunca kesintisiz suruyorsa acil
// bildirim gider. Ardisik iki alarm arasinda STREAK_GAP'ten uzun bosluk varsa kesinti bitmis sayilir.
// Her kesinti icin tek bildirim. Durum bellekte tutulur; yeniden baslatmada sifirlanir (kesinti en
// fazla bir kez daha bildirilir).

const defaultStreakSubjects = "InventoryStockJobErrorAlert"

type streakState struct {
	first, last time.Time
	alerted     bool
}

type streaks struct {
	mu  sync.Mutex
	by  map[string]*streakState
	min time.Duration
	gap time.Duration
	loc *time.Location
}

func newStreaks(minDur, gap time.Duration, loc *time.Location) *streaks {
	return &streaks{by: map[string]*streakState{}, min: minDur, gap: gap, loc: loc}
}

func (r rules) isStreakAlert(m message) bool {
	s := lowerTR(m.Subject)
	for _, k := range r.StreakSubjects {
		if strings.Contains(s, lowerTR(k)) {
			return true
		}
	}
	return false
}

// observe: alarmi kaydeder ve karari dondurur. Zaman olarak mailin gelis zamani kullanilir; boylece
// sunucu kapaliyken biriken mailler de dogru degerlendirilir.
func (s *streaks) observe(m message, from string) decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := from + "|" + lowerTR(m.Subject)
	at := m.ReceivedDateTime
	st := s.by[key]
	if st == nil || at.Sub(st.last) > s.gap {
		st = &streakState{first: at}
		s.by[key] = st
	}
	if at.After(st.last) {
		st.last = at
	}
	dur := st.last.Sub(st.first)
	if dur >= s.min && !st.alerted {
		st.alerted = true
		return decision{Level: levelUrgent, Source: "kural:süreklilik",
			Reason: fmt.Sprintf("%s: hata %d dakikadır kesintisiz sürüyor (%s'ten beri). %s", tenantOf(from),
				int(dur.Minutes()), st.first.In(s.loc).Format("15:04"), truncateRunes(firstLine(strings.TrimSpace(m.Body.Content)), 120))}
	}
	src := fmt.Sprintf("kural:süreklilik (%d dk)", int(dur.Minutes()))
	if st.alerted {
		src += " (bildirildi)"
	}
	return decision{Level: levelNone, MarkRead: true, Source: src}
}

// tenantOf: "info.klaud@clmail..." -> "klaud"; "x@clmail.skechers.com.tr" -> "skechers".
func tenantOf(addr string) string {
	local, domain, _ := strings.Cut(addr, "@")
	if t, ok := strings.CutPrefix(local, "info."); ok {
		return t
	}
	parts := strings.Split(domain, ".")
	if len(parts) > 1 && parts[0] == "clmail" {
		return parts[1]
	}
	return addr
}
