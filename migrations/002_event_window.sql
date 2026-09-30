-- Mix-level event window. NULL means that side is unbounded.
-- Bounds match store.MaxWindowMonths.
ALTER TABLE feeds ADD COLUMN past_months INTEGER;
ALTER TABLE feeds ADD COLUMN future_months INTEGER;

ALTER TABLE feeds ADD CONSTRAINT feeds_past_months_check
    CHECK (past_months IS NULL OR (past_months >= 0 AND past_months <= 120));
ALTER TABLE feeds ADD CONSTRAINT feeds_future_months_check
    CHECK (future_months IS NULL OR (future_months >= 0 AND future_months <= 120));
