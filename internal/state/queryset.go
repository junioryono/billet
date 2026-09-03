package state

import (
	"context"
	"database/sql"
	"errors"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// THE GENERATED QUERIES ARE BOUND HERE, AND EVERY LEDGER-WRITING PACKAGE BINDS
// THEM THROUGH THIS FILE.
//
// sqlc owns exactly one thing: turning a named statement in
// internal/state/queries into a typed Go call. Everything this package exists
// for -- the process lock, the single writer, BEGIN IMMEDIATE, the busy retry,
// the durability pragmas, the maintenance fence and migrations -- stays here.
//
// ALL LEDGER SQL LIVES IN ONE QUERY DIRECTORY, including internal/alloc's and
// internal/rollout's, and that is what makes the guards worth having: the
// prepare-against-the-migrated-schema check, the read/write classification, the
// ASCII rule and the wildcard ban are written once and cover every domain. Those
// two packages therefore need a handle, so ReadQueries, WriteQueries, ReadOps
// and WriteOps are exported and depguard admits them as importers of
// internal/state/ledgerdb -- for its parameter and row types, never to construct
// a *Queries of their own. Nothing else may import it, because a *Queries in a
// caller's hands is the whole ledger with none of the above.
//
// WHY THE READ SIDE IS ADAPTED RATHER THAN WIDENED. sqlc's generated DBTX
// requires PrepareContext even with emit_prepared_queries: false -- measured, not
// assumed -- and Querier deliberately has two methods, because handing out
// anything wider would let a caller write through a connection that is supposed
// to be read-only. Widening Querier to satisfy DBTX would give that away for a
// code-generation convenience: a prepared INSERT is still an INSERT.
//
// So Querier is adapted instead. readOnlyDBTX forwards the two read methods and
// refuses the two write ones, which means a read handle cannot mutate even by
// accident, and the refusal is billet's own sentence rather than SQLite's
// "attempt to write a readonly database".
//
// AND THE SPLIT IS RESTORED AT COMPILE TIME by the two interfaces below. One
// generated *Queries carries every method, so binding it to the reader would
// have turned a compile-time separation into a runtime one; ReadQueries hands
// back ReadOps, which has no mutation on it at all.

// errReadOnlyHandle is what a mutating query gets when it reaches a handle that
// came from the query-only pool.
//
// A SENTINEL SO A TEST CAN ASSERT THE SEPARATION IS REAL, rather than asserting
// that "an error" came back -- which SQLite's own readonly refusal would also
// satisfy, from a different layer, for a different reason.
var errReadOnlyHandle = errors.New(
	"state: a mutating query was issued on a read-only handle; writes go through DB.Tx")

// ReadOps is every generated query that only reads.
//
// HAND-WRITTEN, AND THAT IS THE POINT: it is what ReadQueries returns, so an
// adapter bound to the query-only pool has no mutation in its method set. Adding
// a query means listing it in exactly one of these two interfaces, which
// TestEveryGeneratedQueryIsClaimedByExactlyOneHalf checks, and putting a
// mutation in the wrong one is caught by
// TestReadOpsHoldsExactlyTheQueriesThatOnlyRead rather than by review.
type ReadOps interface {
	AnyBarrierRunExists(ctx context.Context) (bool, error)
	BarrierRunExists(ctx context.Context, node string) (bool, error)
	CertRevocation(ctx context.Context, serial string) (int64, error)
	CountActiveRunnerLeases(ctx context.Context, tier string) (int64, error)
	CountCacheBlocks(ctx context.Context, arg ledgerdb.CountCacheBlocksParams) (int64, error)
	CountForceDestroyInState(ctx context.Context, state string) (int64, error)
	CountForceTargetsInState(ctx context.Context, arg ledgerdb.CountForceTargetsInStateParams) (int64, error)
	CountLiveWorkOnNode(ctx context.Context, arg ledgerdb.CountLiveWorkOnNodeParams) (int64, error)
	CountOpenInTier(ctx context.Context, tier string) (int64, error)
	CountOpenPerTier(ctx context.Context) ([]ledgerdb.CountOpenPerTierRow, error)
	CountOutstandingLeasesOnNode(ctx context.Context, node string) (int64, error)
	DisruptableLease(ctx context.Context, id string) ([]string, error)
	DisruptableLeasesOnNode(ctx context.Context, node string) ([]string, error)
	FleetClaimHolder(ctx context.Context, arg ledgerdb.FleetClaimHolderParams) (string, error)
	ForceDestroyInState(ctx context.Context, state string) (ledgerdb.ForceDestroy, error)
	ForceTargets(ctx context.Context, generation int64) ([]ledgerdb.ForceTargetsRow, error)
	HighestForceDestroyGeneration(ctx context.Context) (int64, error)
	HighestRolloutGeneration(ctx context.Context) (int64, error)
	HostReportsCompute(ctx context.Context, node string) (bool, error)
	LatestForceDestroy(ctx context.Context) (ledgerdb.ForceDestroy, error)
	ListAdmissionRows(ctx context.Context) ([]ledgerdb.Admission, error)
	ListAppliedMigrations(ctx context.Context) ([]ledgerdb.ListAppliedMigrationsRow, error)
	ListAttributedFailures(ctx context.Context, arg ledgerdb.ListAttributedFailuresParams) ([]ledgerdb.ListAttributedFailuresRow, error)
	ListBarrierRuns(ctx context.Context, barrierID string) ([]ledgerdb.ListBarrierRunsRow, error)
	ListCodeBuildRegistrationPaths(ctx context.Context, provider string) ([]ledgerdb.ListCodeBuildRegistrationPathsRow, error)
	ListCredentialSweeps(ctx context.Context) ([]ledgerdb.CredentialSweep, error)
	ListEnrollments(ctx context.Context) ([]ledgerdb.NodeEnrollment, error)
	ListEnrollmentsInState(ctx context.Context, state string) ([]ledgerdb.NodeEnrollment, error)
	ListExpiredLeases(ctx context.Context, arg ledgerdb.ListExpiredLeasesParams) ([]ledgerdb.ListExpiredLeasesRow, error)
	ListFleetClearance(ctx context.Context) ([]ledgerdb.ListFleetClearanceRow, error)
	ListForceDestroyCandidates(ctx context.Context, arg ledgerdb.ListForceDestroyCandidatesParams) ([]ledgerdb.ListForceDestroyCandidatesRow, error)
	ListHeldLeases(ctx context.Context, arg ledgerdb.ListHeldLeasesParams) ([]ledgerdb.ListHeldLeasesRow, error)
	ListHostsReportingCompute(ctx context.Context) ([]string, error)
	ListJobConclusionsForRequest(ctx context.Context, requestID sql.NullInt64) ([]sql.NullString, error)
	ListJobHistory(ctx context.Context, maxRows int64) ([]ledgerdb.ListJobHistoryRow, error)
	ListJoinTokenHashes(ctx context.Context) ([]string, error)
	ListJoinTokens(ctx context.Context) ([]ledgerdb.ListJoinTokensRow, error)
	ListLeaseIDsOnNode(ctx context.Context, arg ledgerdb.ListLeaseIDsOnNodeParams) ([]string, error)
	ListNodeInventories(ctx context.Context) ([]ledgerdb.ListNodeInventoriesRow, error)
	ListNodeWireVersions(ctx context.Context) ([]ledgerdb.ListNodeWireVersionsRow, error)
	ListOutstandingLeases(ctx context.Context) ([]ledgerdb.ListOutstandingLeasesRow, error)
	ListOutstandingRemoteShapes(ctx context.Context) ([]ledgerdb.ListOutstandingRemoteShapesRow, error)
	ListPendingCompletions(ctx context.Context, tier string) ([]ledgerdb.ListPendingCompletionsRow, error)
	ListPlaceableNodes(ctx context.Context) ([]ledgerdb.ListPlaceableNodesRow, error)
	ListPoolRunnersInTier(ctx context.Context, tier string) ([]ledgerdb.PoolRunner, error)
	ListQuarantinedLeaseIDsOn(ctx context.Context, arg ledgerdb.ListQuarantinedLeaseIDsOnParams) ([]string, error)
	ListQuarantinedLeases(ctx context.Context, phase string) ([]ledgerdb.ListQuarantinedLeasesRow, error)
	ListRegisteredNodes(ctx context.Context) ([]ledgerdb.ListRegisteredNodesRow, error)
	ListRemoteCostNodes(ctx context.Context) ([]ledgerdb.ListRemoteCostNodesRow, error)
	ListRevokedCerts(ctx context.Context) ([]ledgerdb.RevokedCert, error)
	ListRolloutHistory(ctx context.Context, maxRows int64) ([]ledgerdb.Rollout, error)
	ReadNewestRolloutForTarget(ctx context.Context, targetDigest string) (ledgerdb.Rollout, error)
	ListRolloutNodePhases(ctx context.Context, rolloutID string) ([]ledgerdb.ListRolloutNodePhasesRow, error)
	ListRolloutNodes(ctx context.Context, rolloutID string) ([]ledgerdb.ListRolloutNodesRow, error)
	ListRunningLeasesWithReplacedHolder(ctx context.Context, arg ledgerdb.ListRunningLeasesWithReplacedHolderParams) ([]ledgerdb.ListRunningLeasesWithReplacedHolderRow, error)
	ListScaleSets(ctx context.Context, org string) ([]ledgerdb.ListScaleSetsRow, error)
	ListServiceableRunnerLeaseIDs(ctx context.Context, tier string) ([]string, error)
	LiveCertsFor(ctx context.Context, arg ledgerdb.LiveCertsForParams) ([]ledgerdb.IssuedCert, error)
	LiveJoinTokenExists(ctx context.Context, arg ledgerdb.LiveJoinTokenExistsParams) (bool, error)
	NodeRevocationCutoff(ctx context.Context, node string) (string, error)
	PendingCompletionMessage(ctx context.Context, arg ledgerdb.PendingCompletionMessageParams) (ledgerdb.PendingCompletionMessageRow, error)
	PendingForceTargets(ctx context.Context, arg ledgerdb.PendingForceTargetsParams) ([]ledgerdb.PendingForceTargetsRow, error)
	ReadAdmission(ctx context.Context) (ledgerdb.ReadAdmissionRow, error)
	ReadBarrierRun(ctx context.Context, arg ledgerdb.ReadBarrierRunParams) (ledgerdb.ReadBarrierRunRow, error)
	ReadComputeBarrier(ctx context.Context) (ledgerdb.ReadComputeBarrierRow, error)
	ReadControllerClaim(ctx context.Context) (ledgerdb.ReadControllerClaimRow, error)
	ReadDeploymentBinding(ctx context.Context) (ledgerdb.ReadDeploymentBindingRow, error)
	ReadNodeHighestRelease(ctx context.Context, name string) (string, error)
	ReadReleaseWatermark(ctx context.Context) (ledgerdb.ReadReleaseWatermarkRow, error)
	ReadEnrollment(ctx context.Context, name string) (ledgerdb.NodeEnrollment, error)
	ReadEnrollmentFingerprint(ctx context.Context, name string) (string, error)
	ReadJobConclusion(ctx context.Context, leaseID string) (sql.NullString, error)
	ReadJobFailureReason(ctx context.Context, leaseID string) (string, error)
	ReadJobNode(ctx context.Context, leaseID string) (sql.NullString, error)
	ReadJobIdentity(ctx context.Context, jobID string) (int64, error)
	ReadJobResult(ctx context.Context, leaseID string) (string, error)
	ReadJobStarted(ctx context.Context, leaseID string) (bool, error)
	ReadLease(ctx context.Context, id string) (ledgerdb.ReadLeaseRow, error)
	ReadLeaseClosure(ctx context.Context, id string) (ledgerdb.ReadLeaseClosureRow, error)
	ReadLeaseEpoch(ctx context.Context, id string) (int64, error)
	ReadLeaseJob(ctx context.Context, id string) (ledgerdb.ReadLeaseJobRow, error)
	ReadLeaseSettlement(ctx context.Context, id string) (ledgerdb.ReadLeaseSettlementRow, error)
	ReadLeaseTargetSize(ctx context.Context, id string) (ledgerdb.ReadLeaseTargetSizeRow, error)
	ReadNodeBarrierFence(ctx context.Context, name string) (ledgerdb.ReadNodeBarrierFenceRow, error)
	ReadNodeCapacity(ctx context.Context, name string) (ledgerdb.ReadNodeCapacityRow, error)
	ReadNodeEpoch(ctx context.Context, name string) (int64, error)
	ReadNodeFence(ctx context.Context, name string) (ledgerdb.ReadNodeFenceRow, error)
	ReadNodeIncarnation(ctx context.Context, name string) (string, error)
	ReadNodeLiveness(ctx context.Context, name string) (int64, error)
	ReadNodeProvider(ctx context.Context, name string) (string, error)
	ReadNodeRegistration(ctx context.Context, name string) (ledgerdb.ReadNodeRegistrationRow, error)
	ReadNodeSize(ctx context.Context, name string) (ledgerdb.ReadNodeSizeRow, error)
	ReadPoolRunnerByLease(ctx context.Context, leaseID string) (ledgerdb.PoolRunner, error)
	ReadPoolRunnerByName(ctx context.Context, runnerName string) (ledgerdb.PoolRunner, error)
	ReadPoolRunnerSettlementByRequest(ctx context.Context, arg ledgerdb.ReadPoolRunnerSettlementByRequestParams) (ledgerdb.ReadPoolRunnerSettlementByRequestRow, error)
	ReadPoolSlotIdentity(ctx context.Context, leaseID string) (int64, error)
	ReadRolloutControllerPhase(ctx context.Context, id string) (string, error)
	ReadRolloutInState(ctx context.Context, state string) (ledgerdb.Rollout, error)
	ReadRolloutNodeProgress(ctx context.Context, arg ledgerdb.ReadRolloutNodeProgressParams) (ledgerdb.ReadRolloutNodeProgressRow, error)
	TotalUsage(ctx context.Context) (ledgerdb.TotalUsageRow, error)
	UsageByNode(ctx context.Context) ([]ledgerdb.UsageByNodeRow, error)
	UsageOnNode(ctx context.Context, node string) (ledgerdb.UsageOnNodeRow, error)
}

// WriteOps is ReadOps plus every generated query that mutates.
//
// It EMBEDS ReadOps because a write transaction reads too: a seal compares the
// admission generation it is about to overwrite, and a force-destroy reads the
// admission row it authorises against, both inside the transaction that writes.
type WriteOps interface {
	ReadOps

	AcknowledgePendingCompletion(ctx context.Context, arg ledgerdb.AcknowledgePendingCompletionParams) error
	AcknowledgePoolRunnerSource(ctx context.Context, arg ledgerdb.AcknowledgePoolRunnerSourceParams) error
	AdvanceRolloutController(ctx context.Context, arg ledgerdb.AdvanceRolloutControllerParams) error
	AdvanceRolloutNode(ctx context.Context, arg ledgerdb.AdvanceRolloutNodeParams) error
	ArchiveJobHistory(ctx context.Context, arg ledgerdb.ArchiveJobHistoryParams) error
	AssignLease(ctx context.Context, arg ledgerdb.AssignLeaseParams) error
	BindDeployment(ctx context.Context, arg ledgerdb.BindDeploymentParams) error
	BindLease(ctx context.Context, arg ledgerdb.BindLeaseParams) error
	BindPoolRunnerJob(ctx context.Context, arg ledgerdb.BindPoolRunnerJobParams) error
	BumpDispatchGeneration(ctx context.Context, name string) (int64, error)
	ClaimController(ctx context.Context, arg ledgerdb.ClaimControllerParams) (int64, error)
	ClaimPoolRunnerForRetirement(ctx context.Context, arg ledgerdb.ClaimPoolRunnerForRetirementParams) (sql.Result, error)
	CompleteForceDestroy(ctx context.Context, arg ledgerdb.CompleteForceDestroyParams) error
	CorrectProvisionalHistory(ctx context.Context, arg ledgerdb.CorrectProvisionalHistoryParams) error
	CorrectProvisionalLease(ctx context.Context, arg ledgerdb.CorrectProvisionalLeaseParams) error
	DecideEnrollment(ctx context.Context, arg ledgerdb.DecideEnrollmentParams) error
	DecommissionNode(ctx context.Context, arg ledgerdb.DecommissionNodeParams) error
	DeleteAcknowledgedCompletion(ctx context.Context, arg ledgerdb.DeleteAcknowledgedCompletionParams) error
	DeleteBarrierRun(ctx context.Context, node string) error
	DeleteCacheBlock(ctx context.Context, arg ledgerdb.DeleteCacheBlockParams) error
	DeleteComputeBarrier(ctx context.Context) error
	DeleteEveryBarrierRun(ctx context.Context) error
	DeleteEveryNodeInventory(ctx context.Context) error
	DeleteMovedScaleSet(ctx context.Context, arg ledgerdb.DeleteMovedScaleSetParams) error
	DeletePoolRunner(ctx context.Context, leaseID string) error
	DeleteRetiredCompletion(ctx context.Context, arg ledgerdb.DeleteRetiredCompletionParams) error
	DeleteScaleSet(ctx context.Context, arg ledgerdb.DeleteScaleSetParams) error
	ExpireLease(ctx context.Context, arg ledgerdb.ExpireLeaseParams) error
	FenceQuarantinedLease(ctx context.Context, arg ledgerdb.FenceQuarantinedLeaseParams) error
	FinishRollout(ctx context.Context, arg ledgerdb.FinishRolloutParams) error
	ForgetEveryNode(ctx context.Context) error
	HeartbeatLease(ctx context.Context, arg ledgerdb.HeartbeatLeaseParams) error
	HoldLease(ctx context.Context, arg ledgerdb.HoldLeaseParams) error
	InsertEnrollment(ctx context.Context, arg ledgerdb.InsertEnrollmentParams) error
	InsertForceDestroy(ctx context.Context, arg ledgerdb.InsertForceDestroyParams) error
	InsertForceDestroyTarget(ctx context.Context, arg ledgerdb.InsertForceDestroyTargetParams) error
	InsertJobIdentity(ctx context.Context, jobID string) error
	InsertJoinToken(ctx context.Context, arg ledgerdb.InsertJoinTokenParams) error
	InsertLease(ctx context.Context, arg ledgerdb.InsertLeaseParams) error
	InsertPoolRunner(ctx context.Context, arg ledgerdb.InsertPoolRunnerParams) error
	InsertPoolSlotIdentity(ctx context.Context, leaseID string) error
	InsertRollout(ctx context.Context, arg ledgerdb.InsertRolloutParams) error
	InsertRolloutNode(ctx context.Context, arg ledgerdb.InsertRolloutNodeParams) error
	MarkLeaseDeregistered(ctx context.Context, id string) error
	MarkLeaseFailure(ctx context.Context, arg ledgerdb.MarkLeaseFailureParams) error
	MarkNodeNotLive(ctx context.Context, arg ledgerdb.MarkNodeNotLiveParams) error
	MarkPoolRunnerBusy(ctx context.Context, arg ledgerdb.MarkPoolRunnerBusyParams) error
	MarkPoolRunnerRetired(ctx context.Context, arg ledgerdb.MarkPoolRunnerRetiredParams) error
	MarkPoolRunnerRetiring(ctx context.Context, arg ledgerdb.MarkPoolRunnerRetiringParams) error
	ReclaimLease(ctx context.Context, arg ledgerdb.ReclaimLeaseParams) error
	RecordCredentialSweep(ctx context.Context, arg ledgerdb.RecordCredentialSweepParams) error
	RefreshLeaseHolder(ctx context.Context, arg ledgerdb.RefreshLeaseHolderParams) error
	RecordBarrierRun(ctx context.Context, arg ledgerdb.RecordBarrierRunParams) error
	RecordHistoryDisruption(ctx context.Context, arg ledgerdb.RecordHistoryDisruptionParams) error
	RecordIssuedCert(ctx context.Context, arg ledgerdb.RecordIssuedCertParams) error
	RecordJobAssignment(ctx context.Context, arg ledgerdb.RecordJobAssignmentParams) error
	RecordJobResult(ctx context.Context, arg ledgerdb.RecordJobResultParams) error
	RecordJobRun(ctx context.Context, arg ledgerdb.RecordJobRunParams) error
	RecordJobStart(ctx context.Context, arg ledgerdb.RecordJobStartParams) error
	BackfillFailureReason(ctx context.Context, arg ledgerdb.BackfillFailureReasonParams) error
	BackfillLeaseFailureReason(ctx context.Context, arg ledgerdb.BackfillLeaseFailureReasonParams) error
	RecordLeaseDisruption(ctx context.Context, arg ledgerdb.RecordLeaseDisruptionParams) error
	RecordMigration(ctx context.Context, arg ledgerdb.RecordMigrationParams) error
	RecordNodeRevocation(ctx context.Context, arg ledgerdb.RecordNodeRevocationParams) error
	ReplaceDeniedEnrollment(ctx context.Context, arg ledgerdb.ReplaceDeniedEnrollmentParams) error
	RequestForceRelease(ctx context.Context, arg ledgerdb.RequestForceReleaseParams) error
	ResizeLease(ctx context.Context, arg ledgerdb.ResizeLeaseParams) error
	RetirePendingCompletion(ctx context.Context, arg ledgerdb.RetirePendingCompletionParams) error
	RevokeCert(ctx context.Context, arg ledgerdb.RevokeCertParams) error
	SetAdmission(ctx context.Context, arg ledgerdb.SetAdmissionParams) error
	SetLeasePhase(ctx context.Context, arg ledgerdb.SetLeasePhaseParams) error
	SettleForceTarget(ctx context.Context, arg ledgerdb.SettleForceTargetParams) (sql.Result, error)
	SetReleaseWatermark(ctx context.Context, arg ledgerdb.SetReleaseWatermarkParams) error
	SpendJoinToken(ctx context.Context, arg ledgerdb.SpendJoinTokenParams) (sql.Result, error)
	StartPoolRunner(ctx context.Context, arg ledgerdb.StartPoolRunnerParams) error
	TerminalizeQuarantinedLease(ctx context.Context, arg ledgerdb.TerminalizeQuarantinedLeaseParams) error
	TerminalizeQuarantinedLeaseWithReason(ctx context.Context, arg ledgerdb.TerminalizeQuarantinedLeaseWithReasonParams) error
	TerminateForcedLease(ctx context.Context, arg ledgerdb.TerminateForcedLeaseParams) error
	UpsertCacheBlock(ctx context.Context, arg ledgerdb.UpsertCacheBlockParams) error
	UpsertComputeBarrier(ctx context.Context, arg ledgerdb.UpsertComputeBarrierParams) error
	UpsertIssuedEnrollment(ctx context.Context, arg ledgerdb.UpsertIssuedEnrollmentParams) error
	UpsertNodeInventory(ctx context.Context, arg ledgerdb.UpsertNodeInventoryParams) error
	UpsertNodeRegistration(ctx context.Context, arg ledgerdb.UpsertNodeRegistrationParams) (int64, error)
	UpsertPendingCompletion(ctx context.Context, arg ledgerdb.UpsertPendingCompletionParams) error
	UpsertScaleSet(ctx context.Context, arg ledgerdb.UpsertScaleSetParams) error
	WithdrawNode(ctx context.Context, arg ledgerdb.WithdrawNodeParams) (int64, error)
}

// The generated set must satisfy the whole contract. A query renamed or removed
// in a .sql file fails here, at compile time, rather than at the call site.
var _ WriteOps = (*ledgerdb.Queries)(nil)

// WriteQueries binds the generated set to a write transaction.
//
// IT GRANTS NO AUTHORITY THE CALLER DID NOT ALREADY HAVE, which is why exporting
// it is safe: whatever a *sql.Tx can do, it can do without this. What it adds is
// the read/write split and one place that lists every query.
//
// IT DOES NOT PROVE THE TRANSACTION CAME FROM DB.Tx, and the difference is worth
// stating rather than implying. In practice DB.Tx is the only source in this
// program -- it is what has taken the process lock, begun IMMEDIATE, consulted the
// maintenance fence and installed the busy retry -- but nothing in the type says
// so, and a caller that opened its own handle could begin its own transaction. The
// driver import that would need is confined to this package by depguard, and every
// statement such a caller made would be reported by the rawsql analyzer, so the
// bypass is guarded elsewhere rather than here.
func WriteQueries(tx *sql.Tx) WriteOps { return ledgerdb.New(tx) }

// ReadQueries binds the read half to anything that can answer a query.
//
// It takes Querier rather than a concrete type so it serves all three readers:
// the query-only pool inside View, a read transaction, and the bare *sql.DB the
// Peek functions open over a ledger file they must not migrate or create.
//
// It returns ReadOps rather than WriteOps, so a caller handed a read handle --
// including one in internal/alloc or internal/rollout -- has no mutation in its
// method set at all. That is the compile-time half; readOnlyDBTX underneath is
// the runtime half.
//
// WHAT IT GUARANTEES IS THE METHOD SET, NOT THE POOL, and the difference matters
// because a *sql.Tx satisfies Querier: a caller inside DB.Tx may legitimately
// bind this to the WRITE connection, which is what a read that must happen inside
// a write transaction needs. Nothing can mutate through it either way.
func ReadQueries(q Querier) ReadOps { return ledgerdb.New(readOnlyDBTX{q: q}) }

// readOnlyDBTX presents a Querier as the interface generated code expects.
//
// The two refusals are the substance. sqlc needs Exec and Prepare on its DBTX
// whether or not any query uses them, and supplying the reader pool directly
// would satisfy both from a *sql.DB that can, at the driver level, do anything.
// query_only(ON) would refuse the write eventually; refusing it here means the
// diagnostic names the layer that made the mistake.
//
// WHAT IT DOES NOT COVER, because assuming otherwise is the mistake to avoid: a
// DML statement declared `:one` or `:many`. `UPDATE … RETURNING` is dispatched
// through QueryRowContext, which this forwards, so the sentinel never fires and
// query_only is what refuses it — measured, as "attempt to write a readonly
// database (8)". Sniffing the SQL text here to catch that would be a second,
// weaker parser of statements billet already has a real classification for, so
// the guarantee is layered instead: TestReadOpsHoldsExactlyTheQueriesThatOnlyRead
// classifies every query from the FIRST KEYWORD of its .sql source and refuses to
// let a mutation be listed in ReadOps at all.
type readOnlyDBTX struct{ q Querier }

func (readOnlyDBTX) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, errReadOnlyHandle
}

func (readOnlyDBTX) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) {
	return nil, errReadOnlyHandle
}

func (r readOnlyDBTX) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	//billet:ignore rawsql // forwards a statement sqlc generated; the text is not this package's
	return r.q.QueryContext(ctx, query, args...)
}

func (r readOnlyDBTX) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	//billet:ignore rawsql // forwards a statement sqlc generated; the text is not this package's
	return r.q.QueryRowContext(ctx, query, args...)
}

// boolInt and intBool convert billet's booleans to the 0/1 the ledger stores.
//
// EXPLICIT RATHER THAN AN sqlc OVERRIDE. The columns are INTEGER NOT NULL with
// CHECK (x IN (0,1)) in STRICT tables, and the driver has always done this
// conversion implicitly; doing it here makes it visible at the seam that has to
// get it right. A go_type override in sqlc.yaml would put the mapping in a file
// nobody reads while editing the adapter, and a wrong one is silent.
func boolInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

func intBool(n int64) bool { return n != 0 }
