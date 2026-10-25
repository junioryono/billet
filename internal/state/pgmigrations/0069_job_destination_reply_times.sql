-- migration 69: job_destination_reply_times
--
-- HOW LONG A DESTINATION'S CONNECTIONS WAITED FOR A FIRST REPLY (#631), beside
-- what migration 68 keeps of what got nothing back: how many of its answered
-- connections were timed, how many of those waited longer than a second, and
-- the longest wait in milliseconds. Timed by the node from the host's
-- connection tracker, from the event that created a flow to its first event
-- that saw a reply. It is where a job stalled when everything answered, only
-- late: a slow resolver, or an uplink shaped until a SYN-ACK came after the
-- guest's first retransmit.
--
-- NULL WHEN IT WAS NOT TIMED, which is not a destination that answered fast:
-- every row from before this migration, every row of a report a node sent over
-- a wire before VersionReplyTimes, and every row of a job whose tracker events
-- were not all read. A report that timed writes a number, 0 included, in all
-- three columns of every one of its rows, and the control plane reads the
-- lease's report as timed only when all of its rows carry them.
--
-- NO CHECK BETWEEN THE COLUMNS, or against connections, which columns added
-- here cannot name in SQLite; the bounds are held in Go, by
-- lease.JobDestination's validation, before a row is written.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_destinations ADD COLUMN timed_replies bigint CHECK (timed_replies >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_destinations ADD COLUMN slow_replies bigint CHECK (slow_replies >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_destinations ADD COLUMN slowest_reply_ms bigint CHECK (slowest_reply_ms >= 0);
-- +billet:end
