-- migration 49: job_placement_and_cache
--
-- WHAT A LEASE WAS CHARGED FOR, AND WHAT THE CACHE DID, kept past the lease.
--
-- job_history keeps the tier and the node; the facts that distinguish one
-- placement from another -- the backend it ran on, the shape placement bought,
-- the charged vCPU and memory, the site, and the price that shape was charged at
-- -- lived only on the lease row, and a lease is reaped. So the moment the row
-- went the deployment could no longer say what it bought for a job or what that
-- job cost, and a cost report had to reprice last month at today's catalogue.
--
-- THE PRICE IS RECORDED ON THE LEASE WHEN THE SHAPE IS CHARGED, at escrow and
-- again on a fallback resize, and copied onto the history when the lease
-- terminalizes. Never read from the node's catalogue at archive time: a node may
-- re-register with new prices while leases are open (the shape comparison that
-- guards re-registration deliberately ignores price), and a row repriced at
-- archive is wrong in a way nobody can see.
--
-- price_micros_per_hour IS MILLIONTHS OF A DOLLAR, the unit config.USDPerHour
-- already stores, because EC2 publishes rates beyond cents. ZERO MEANS NO PRICE
-- WAS RECORDED, and the row's other columns say why: an empty instance_type is a
-- host-backed lease that bought nothing, and a remote instance_type beside zero
-- is a row written before this column existed. A reader renders the second as
-- unknown, never as $0.
--
-- site IS THE PLACED HOST'S REGISTERED SITE AT ESCROW, recorded on the lease for
-- the same reason target_node is: a host does not move while it has work, so it
-- could be read back at archive, but a fact placement decided on belongs on the
-- row placement wrote.
--
-- THE CACHE COLUMNS ARE OBSERVATIONS, in a closed vocabulary the node writes
-- from what it saw rather than what the tier intended: image_cache is what the
-- guest's Docker image store clone did (warm, cold, unavailable, unused) and
-- cache_generation the generation a warm clone resolved; actions_cache is what
-- the Actions cache interception did for the first CacheService request (served,
-- spliced, disabled by the kill switch, unavailable, off, unused), recorded
-- once the disposition is final rather than when it is intended. The FIRST observation is
-- kept, on the lease and on the history row alike, and it is written onto the
-- history row the moment it is observed rather than left to the archive, for the
-- reason migration 35 gives for a disruption. The empty string is the zero value
-- and means nothing was observed.
--
-- NO CHECK ON THE VOCABULARIES, for the reason migration 35 gives: SQLite cannot
-- extend a column CHECK in place. The sets are closed in Go by
-- alloc.ImageCache.Valid and alloc.ActionsCache.Valid, which every new
-- observation goes through; an archive carries whatever the lease already holds.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN site TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN price_micros_per_hour INTEGER NOT NULL DEFAULT 0 CHECK (price_micros_per_hour >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN image_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN cache_generation TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN actions_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN chosen_provider TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN instance_type TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN vcpu INTEGER NOT NULL DEFAULT 0 CHECK (vcpu >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN memory INTEGER NOT NULL DEFAULT 0 CHECK (memory >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN site TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN price_micros_per_hour INTEGER NOT NULL DEFAULT 0 CHECK (price_micros_per_hour >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN image_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN cache_generation TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN actions_cache TEXT NOT NULL DEFAULT ''
-- +billet:end
