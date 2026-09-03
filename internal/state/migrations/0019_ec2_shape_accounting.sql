-- migration 19: ec2_shape_accounting
--
-- AN EC2 BUDGET IS CHARGED FOR WHAT THE NODE MAY BUY, not merely what a tier
-- requested. The ordered shape list belongs to the node row because placement
-- happens on the control plane, while requested_* stays immutable on the lease
-- so a fallback can change the charged size without forgetting what must fit.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN ec2_shapes TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN requested_vcpu INTEGER NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN requested_memory INTEGER NOT NULL DEFAULT 0 CHECK (requested_memory >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN instance_type TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
UPDATE leases SET requested_vcpu = vcpu, requested_memory = memory
		  WHERE requested_vcpu = 0 OR requested_memory = 0
-- +billet:end
