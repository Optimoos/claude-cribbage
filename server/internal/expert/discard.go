package expert

import "cribbage/internal/engine"

// discarder values each discard option as the probability of winning the
// game, through the hand that follows and the win table. It keeps scratch
// space, so it is not safe for concurrent use.
type discarder struct {
	win     *WinTable
	t       *DiscardTables
	noFixes bool // M14's discards: heels with the show, even odds when both peg out
	// keep, when not nil, conditions the opponent's show and the crib on
	// their likely hands (conddiscard.go) through post.
	keep *keepModel
	post *condPost

	// Per decision.
	me, opp int
	iDeal   bool

	// Per option: distributions over the starters I cannot see.
	pm, po  []float32                 // my and the opponent's pegging points
	qmix    [maxShow]float64          // the opponent's show
	hand    [maxShow]float64          // my show (dealer: my show plus my crib)
	cribByH [maxShow][maxShow]float64 // pone: P(my show, their crib)
	// pone: P(my show, their show plus their crib)
	theirsByH [maxShow][2 * maxShow]float64
	// dealer: g[mp][j] is my win probability after my show and crib, given
	// my pegging mp and their pegging plus show j.
	g [pegBins][pegBins + maxShow]float64
}

func newDiscarder(win *WinTable, t *DiscardTables) *discarder {
	return &discarder{win: win, t: t}
}

// choose returns the discard in legal with the highest win probability;
// ties go to the earliest.
func (d *discarder) choose(o *engine.Observation, legal []engine.Action) engine.Action {
	me := o.Player
	d.me, d.opp = o.Scores[me], o.Scores[1-me]
	d.iDeal = o.Dealer == me
	hand := o.HandCards()
	var held [52]bool
	for _, c := range hand {
		held[c] = true
	}
	if d.keep != nil {
		if d.post == nil {
			d.post = new(condPost)
		}
		var six [6]engine.Card
		copy(six[:], hand)
		oside := 0
		if d.iDeal {
			oside = 1
		}
		d.post.posterior(d.keep, oside, six)
	}
	best, bestV := legal[0], -1.0
	for _, a := range legal {
		var kept [4]engine.Card
		n := 0
		for _, c := range hand {
			if c != a.Cards[0] && c != a.Cards[1] {
				kept[n] = c
				n++
			}
		}
		if v := d.value(kept, a.Cards, &held); v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// heelsOrderFrom is the score from which a discard values the dealer's
// heels (two for a jack starter) before pegging, as they are scored, rather
// than with the show. Below it no one can reach 121 in the hand, so the
// order does not matter and the cheaper single pass is exact enough.
const heelsOrderFrom = 90

// value is my win probability after keeping kept and discarding disc.
func (d *discarder) value(kept [4]engine.Card, disc [2]engine.Card, held *[52]bool) float64 {
	if d.noFixes || max(d.me, d.opp) < heelsOrderFrom {
		return d.valueOver(kept, disc, held, func(engine.Card) bool { return true }, true)
	}
	// Jack starters: the dealer scores two at the cut, which may win.
	var nJ, nOther float64
	for s := engine.Card(0); s < 52; s++ {
		if !held[s] {
			if s.Rank() == 11 {
				nJ++
			} else {
				nOther++
			}
		}
	}
	vOther := d.valueOver(kept, disc, held, func(s engine.Card) bool { return s.Rank() != 11 }, false)
	var vJ float64
	me, opp := d.me, d.opp
	if d.iDeal {
		d.me += 2
	} else {
		d.opp += 2
	}
	switch {
	case d.me >= engine.WinScore:
		vJ = 1
	case d.opp >= engine.WinScore:
		vJ = 0
	default:
		vJ = d.valueOver(kept, disc, held, func(s engine.Card) bool { return s.Rank() == 11 }, false)
	}
	d.me, d.opp = me, opp
	return (nJ*vJ + nOther*vOther) / (nJ + nOther)
}

// valueOver is my win probability over the starters in that I cannot see,
// counting the heels with the show when heelsInShow.
func (d *discarder) valueOver(kept [4]engine.Card, disc [2]engine.Card, held *[52]bool, in func(engine.Card) bool, heelsInShow bool) float64 {
	side := 1
	if d.iDeal {
		side = 0
	}
	shape := shapeOf(kept)
	d.pm = trim(d.t.peg(side, shape, 0))
	d.po = trim(d.t.peg(side, shape, 1))
	if d.keep != nil {
		d.condOption(kept, disc, held, in, heelsInShow)
	} else {
		d.tableOption(kept, disc, held, in, heelsInShow)
	}
	d.dealerTable()
	return d.pegValue()
}

// tableOption fills the per-option distributions from the show and crib
// tables.
func (d *discarder) tableOption(kept [4]engine.Card, disc [2]engine.Card, held *[52]bool, in func(engine.Card) bool, heelsInShow bool) {
	side, oside := 1, 0
	if d.iDeal {
		side, oside = 0, 1
	}
	pair := pairOf(disc[0], disc[1])

	clear(d.qmix[:])
	clear(d.hand[:])
	clear(d.cribByH[:])
	var nIn float64
	for s := engine.Card(0); s < 52; s++ {
		if !held[s] && in(s) {
			nIn++
		}
	}
	w := 1 / nIn
	for s := engine.Card(0); s < 52; s++ {
		if held[s] || !in(s) {
			continue
		}
		sr := s.Rank()
		h := engine.ScoreHand(kept, s, false).Total()
		heels := 0
		if sr == 11 && heelsInShow {
			heels = 2 // scored by the dealer at the cut; counted with the show here
		}
		for q, p := range d.t.show(oside, sr) {
			d.qmix[q] += w * float64(p)
		}
		crib := d.t.crib(side, pair, sr)
		if d.iDeal {
			// My show and my crib are counted together, after theirs.
			for c, p := range crib {
				if p != 0 {
					d.hand[min(h+heels+c, maxShow-1)] += w * float64(p)
				}
			}
			continue
		}
		d.hand[h] += w
		row := &d.cribByH[h]
		for c, p := range crib {
			row[min(c+heels, maxShow-1)] += w * float64(p)
		}
	}

	if !d.iDeal {
		for h := range d.cribByH {
			conv := &d.theirsByH[h]
			clear(conv[:])
			if d.hand[h] == 0 {
				continue
			}
			for c, pc := range d.cribByH[h] {
				if pc == 0 {
					continue
				}
				for q, pq := range d.qmix {
					conv[c+q] += pc * pq
				}
			}
		}
	}
}

// dealerTable fills d.g for the dealer from d.hand.
func (d *discarder) dealerTable() {
	if d.iDeal {
		for mp := range d.pm {
			a := d.me + mp
			for j := 0; j < len(d.po)+maxShow-1; j++ {
				var x float64
				for m, pm := range d.hand {
					if pm == 0 {
						continue
					}
					if a+m >= engine.WinScore {
						x += pm
					} else {
						x += pm * d.win.Win(a+m, d.opp+j, false)
					}
				}
				d.g[mp][j] = x
			}
		}
	}
}

// pegValue is the option's win probability over both players' pegging.
func (d *discarder) pegValue() float64 {
	var v float64
	for mp, pmp := range d.pm {
		if pmp == 0 {
			continue
		}
		a := d.me + mp
		for op, pop := range d.po {
			if pop == 0 {
				continue
			}
			b := d.opp + op
			var x float64
			switch {
			case a >= engine.WinScore && b >= engine.WinScore && d.noFixes:
				x = 0.5
			case a >= engine.WinScore && b >= engine.WinScore:
				x = pegOutFirst(engine.WinScore-d.me, mp, engine.WinScore-d.opp, op)
			case a >= engine.WinScore:
				x = 1
			case b >= engine.WinScore:
				x = 0
			case d.iDeal:
				x = d.afterPegDealer(mp, op)
			default:
				x = d.afterPegPone(a, b)
			}
			v += float64(pmp) * float64(pop) * x
		}
	}
	return v
}

// afterPegDealer: their show, then my show and crib together (d.g), then
// they deal.
func (d *discarder) afterPegDealer(mp, op int) float64 {
	var v float64
	for q, pq := range d.qmix {
		if pq != 0 && d.opp+op+q < engine.WinScore {
			v += pq * d.g[mp][op+q]
		}
	}
	return v
}

// afterPegPone: my show, then their show and their crib, then I deal.
func (d *discarder) afterPegPone(a, b int) float64 {
	var v float64
	for h, ph := range d.hand {
		if ph == 0 {
			continue
		}
		if a+h >= engine.WinScore {
			v += ph
			continue
		}
		for t, pt := range d.theirsByH[h] {
			if pt != 0 && b+t < engine.WinScore {
				v += pt * d.win.Win(a+h, b+t, true)
			}
		}
	}
	return v
}

// trim drops a distribution's negligible tail.
func trim(p []float32) []float32 {
	n := len(p)
	for n > 1 && p[n-1] < 1e-5 {
		n--
	}
	return p[:n]
}

// pegOutFirst is the chance I peg out first when both of us reach 121 in
// the pegging: I need myNeed of my myPeg points and they theirNeed of
// their theirPeg. Pegging points come at a roughly steady rate through the
// play, so whoever needs the smaller share of their points tends to get
// there first.
func pegOutFirst(myNeed, myPeg, theirNeed, theirPeg int) float64 {
	a := float64(myNeed) / float64(max(myPeg, 1))
	b := float64(theirNeed) / float64(max(theirPeg, 1))
	switch {
	case a < b:
		return 1
	case a > b:
		return 0
	}
	return 0.5
}
