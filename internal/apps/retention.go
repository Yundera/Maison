package apps

// RetentionModel says what *kind* of expiry an engine's storage can survive.
//
// It is deliberately three separate questions, and this is only the first:
//
//   - What deletions are sound?      RetentionModel, here.
//   - Who performs them?             Caps.SelfExpiring.
//   - What did the user ask for?     backupconfig.Mode.
//
// They are independent, and confusing the first with the other two destroys data
// rather than merely disappointing someone. A content-addressed repository can drop
// any snapshot in its history and the ones either side of it stay restorable. A
// mirror that keeps history by moving displaced files into a dated folder cannot:
// reconstructing an old state means overlaying every generation from that point
// forward, so deleting a generation in the middle does not lose that one day — it
// breaks every restore point behind it. Tiered (grandfather-father-son) retention is
// therefore *unsound* on that storage, and nothing about the deletion looks wrong
// until someone tries to restore.
//
// So the engine declares the model and the planner refuses to produce a deletion the
// storage cannot survive. See internal/backup/retention.
type RetentionModel string

const (
	// RetentionNone is the zero value, and it expires nothing. An engine that has not
	// declared a model accumulates backups forever, which is the conservative answer:
	// the failure mode is a bill, not a lost restore point.
	RetentionNone RetentionModel = ""

	// RetentionSnapshot is history as independent, individually deletable generations
	// — a content-addressed repository (kopia, restic) or a directory of self-contained
	// archives (the local engine). Any generation may be dropped, so tiered retention
	// works in full.
	RetentionSnapshot RetentionModel = "snapshot"

	// RetentionChain is history as a chain, where each generation is meaningful only
	// with every generation newer than it — an rclone mirror plus a dated --backup-dir
	// being the case in hand. Only the oldest tail may be truncated. Tiers are not
	// expressible and must not be faked: a planner asked for them keeps everything
	// newer than the oldest generation the tiers would have kept.
	RetentionChain RetentionModel = "chain"

	// RetentionLifecycle is history the storage expires by itself — object versioning
	// under a bucket lifecycle rule. Maison configures nothing and deletes nothing;
	// the only retention such storage can express is an age, and the bucket applies it.
	RetentionLifecycle RetentionModel = "lifecycle"
)
