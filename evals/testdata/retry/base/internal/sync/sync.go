package sync

func (s *Syncer) flush(ctx context.Context) error {
	if err := s.w.Commit(ctx, s.pending); err != nil {
		return fmt.Errorf("sync: fatal: %w", err)
	}
	s.pending = Batch{}
	return nil
}
