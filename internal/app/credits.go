package app

import (
	"sort"
	"time"
)

const (
	nanoCreditsPerCredit = 1_000_000_000
	routedCreditHistory  = 8 * 24 * time.Hour
)

type routedCreditPoint struct {
	at            time.Time
	nanoCredits   int64
	unpriced      int64
	totalCredits  int64
	totalUnpriced int64
}

func (a *accountStats) addRoutedCredits(at time.Time, nanoCredits int64, known bool) {
	point := routedCreditPoint{at: at, nanoCredits: nanoCredits}
	if !known {
		point.unpriced = 1
	}
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return a.routedCredits[index].at.After(at)
	})
	a.routedCredits = append(a.routedCredits, routedCreditPoint{})
	copy(a.routedCredits[index+1:], a.routedCredits[index:])
	a.routedCredits[index] = point
	var credits, unpriced int64
	if index > 0 {
		credits = a.routedCredits[index-1].totalCredits
		unpriced = a.routedCredits[index-1].totalUnpriced
	}
	for current := index; current < len(a.routedCredits); current++ {
		credits += a.routedCredits[current].nanoCredits
		unpriced += a.routedCredits[current].unpriced
		a.routedCredits[current].totalCredits = credits
		a.routedCredits[current].totalUnpriced = unpriced
	}
	a.pruneRoutedCredits(a.routedCredits[len(a.routedCredits)-1].at.Add(-routedCreditHistory))
}

func (a *accountStats) pruneRoutedCredits(cutoff time.Time) {
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return !a.routedCredits[index].at.Before(cutoff)
	})
	if index < 1_024 || index*2 < len(a.routedCredits) {
		return
	}
	index--
	copy(a.routedCredits, a.routedCredits[index:])
	a.routedCredits = a.routedCredits[:len(a.routedCredits)-index]
}

func (a *accountStats) routedCreditsSince(start time.Time) (float64, time.Time, bool) {
	if len(a.routedCredits) == 0 || start.IsZero() {
		return 0, time.Time{}, false
	}
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return !a.routedCredits[index].at.Before(start)
	})
	last := a.routedCredits[len(a.routedCredits)-1]
	var credits, unpriced int64
	if index > 0 {
		before := a.routedCredits[index-1]
		credits = last.totalCredits - before.totalCredits
		unpriced = last.totalUnpriced - before.totalUnpriced
	} else {
		first := a.routedCredits[0]
		credits = last.totalCredits - first.totalCredits + first.nanoCredits
		unpriced = last.totalUnpriced - first.totalUnpriced + first.unpriced
	}
	if unpriced > 0 {
		return 0, time.Time{}, false
	}
	since := start
	if index == 0 && a.routedCredits[0].at.After(start) {
		since = a.routedCredits[0].at
	}
	return float64(credits) / nanoCreditsPerCredit, since, true
}
