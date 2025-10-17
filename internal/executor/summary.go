package executor

type Summary struct {
	Executed int
	Cached   int
	Skipped  int
	Failed   map[string]error
}

func (s *Summary) Merge(other Summary) {
	s.Executed += other.Executed
	s.Cached += other.Cached
	s.Skipped += other.Skipped
	if other.Failed != nil {
		if s.Failed == nil {
			s.Failed = make(map[string]error)
		}
		for k, v := range other.Failed {
			s.Failed[k] = v
		}
	}
}
