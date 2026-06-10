package dispatch

// SpawnHandle identifies one spawned hand — a fresh-context worker
// running AS a derived identity of its parent aspect (`<parent>.sub-N`,
// roundtable P2 / NEX-571). RunID is empty when the hand is accepted
// but queued (per-parent spawn cap or global MaxConc); it launches when
// capacity frees, mirroring Submit's queue semantics.
type SpawnHandle struct {
	RunID string
	Name  string
}
