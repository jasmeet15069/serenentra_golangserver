-- ONE-OFF, ALREADY EXECUTED. Retained as an audit record, not a tool to re-run
-- casually.
--
--   Run against : tenant testingxyz (hotel_id 4212e55d-7415-41be-b763-7bd4c4cb0a85)
--   Run on      : 2026-08-05
--   Result      : 4 journals, SAL-000001..SAL-000004, INR 5,074.00, balanced
--
-- Kept byte-for-byte as executed so this file matches what actually hit the
-- books. The hotel_id is therefore hardcoded rather than parameterised. It is
-- idempotent and safe to re-run (it posts nothing on a second pass), but any
-- other tenant needs its own reviewed copy.
--
-- Rehearse before committing anything similar:
--   sed 's/^COMMIT;/ROLLBACK;/' <file> | psql ...
--
-- Backfill the four historical POS sales into the general ledger.
--
-- These orders were settled through the legacy /pos/orders path before it
-- posted to the ledger, so INR 5,074 of revenue never reached the books. The
-- code path is fixed going forward; this recovers the history.
--
-- Each order is posted exactly as postSalesToLedger would have, so the entries
-- are indistinguishable from live ones:
--
--   Dr 1000 Cash            total
--     Cr 4100 F&B Revenue     total - tax
--     Cr 2100 GST Payable     tax
--
-- Cash (1000) rather than Bank (1010): pos_orders records no payment method,
-- and the live legacy path defaults to cash for exactly that reason.
--
-- entry_date is the ORDER's date, not today. Revenue belongs to the period it
-- was earned; dating it today would misstate both July and August.
--
-- Idempotent: an order whose "ORDER <number>" reference already exists is
-- skipped, so re-running cannot double-book revenue.

BEGIN;

DO $backfill$
DECLARE
    h          UUID := '4212e55d-7415-41be-b763-7bd4c4cb0a85';
    o          RECORD;
    v_no       BIGINT;
    v_str      TEXT;
    entry      UUID;
    acc_cash   UUID;
    acc_rev    UUID;
    acc_gst    UUID;
    n_posted   INT := 0;
    sum_total  NUMERIC(14,2) := 0;
BEGIN
    SELECT id INTO acc_cash FROM accounting_accounts WHERE hotel_id = h AND code = '1000';
    SELECT id INTO acc_rev  FROM accounting_accounts WHERE hotel_id = h AND code = '4100';
    SELECT id INTO acc_gst  FROM accounting_accounts WHERE hotel_id = h AND code = '2100';

    IF acc_cash IS NULL OR acc_rev IS NULL OR acc_gst IS NULL THEN
        RAISE EXCEPTION 'chart of accounts incomplete: cash=% revenue=% gst=%',
            acc_cash, acc_rev, acc_gst;
    END IF;

    FOR o IN
        SELECT p.order_number,
               p.created_at::date AS entry_date,
               ROUND(p.tax, 2)    AS tax,
               ROUND(p.total, 2)  AS total
        FROM pos_orders p
        WHERE p.hotel_id = h
          AND lower(p.status) IN ('paid', 'completed', 'settled', 'closed')
          AND ROUND(p.total, 2) <> 0
          AND NOT EXISTS (
              SELECT 1 FROM accounting_journal_entries e
              WHERE e.hotel_id = h AND e.reference = 'ORDER ' || p.order_number
          )
        ORDER BY p.created_at
    LOOP
        -- Allocate a voucher from the same counter live postings use, so the
        -- backfilled entries sit in one continuous SAL series.
        INSERT INTO accounting_voucher_counters (hotel_id, voucher_type, next_no)
        VALUES (h, 'SAL', 2)
        ON CONFLICT (hotel_id, voucher_type)
        DO UPDATE SET next_no = accounting_voucher_counters.next_no + 1
        RETURNING next_no - 1 INTO v_no;

        v_str := 'SAL-' || lpad(v_no::text, 6, '0');
        entry := gen_random_uuid();

        INSERT INTO accounting_journal_entries
            (id, hotel_id, entry_date, description, reference, voucher_no, voucher_type, created_at)
        VALUES
            (entry, h, o.entry_date,
             'POS sale - order ' || o.order_number || ' (backfilled)',
             'ORDER ' || o.order_number, v_str, 'SAL', now());

        INSERT INTO accounting_journal_lines
            (id, entry_id, hotel_id, account_id, debit, credit, memo, created_at)
        VALUES
            (gen_random_uuid(), entry, h, acc_cash, o.total, 0, 'Sale settlement (cash)', now()),
            (gen_random_uuid(), entry, h, acc_rev, 0, o.total - o.tax, 'F&B revenue', now()),
            (gen_random_uuid(), entry, h, acc_gst, 0, o.tax, 'GST on sale', now());

        n_posted  := n_posted + 1;
        sum_total := sum_total + o.total;
        RAISE NOTICE 'posted % as % on % for %', o.order_number, v_str, o.entry_date, o.total;
    END LOOP;

    RAISE NOTICE 'backfill complete: % entries, total %', n_posted, sum_total;
END
$backfill$;

-- Refuse to commit an unbalanced ledger.
DO $guard$
DECLARE
    d NUMERIC(14,2);
    c NUMERIC(14,2);
BEGIN
    SELECT COALESCE(SUM(debit), 0), COALESCE(SUM(credit), 0) INTO d, c
    FROM accounting_journal_lines
    WHERE hotel_id = '4212e55d-7415-41be-b763-7bd4c4cb0a85';

    IF d <> c THEN
        RAISE EXCEPTION 'ledger does not balance after backfill: debits % credits %', d, c;
    END IF;
    RAISE NOTICE 'balance check passed: debits = credits = %', d;
END
$guard$;

COMMIT;
