-- Evidence assertions (canonical table: evidence_assertions). Evidence is a
-- directed, typed relation between object versions — separate from the RSG
-- relation graph (invariant 10: Provenance Graph != Evidence Graph).

-- name: CreateEvidenceAssertion :one
INSERT INTO evidence_assertions
    (project_id, state_id, target_object_version_id, evidence_object_version_id,
     relation_type, evidence_type, scope, directness, inference_nature,
     reasoning_note, created_by)
VALUES
    (@project_id, @state_id, @target_object_version_id, @evidence_object_version_id,
     @relation_type, @evidence_type, @scope, @directness, @inference_nature,
     @reasoning_note, @created_by)
RETURNING *;

-- name: ListEvidenceAssertionsForTarget :many
SELECT * FROM evidence_assertions
WHERE target_object_version_id = @object_version_id
ORDER BY created_at, id;
