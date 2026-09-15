-- Project template instantiations (canonical table
-- project_template_instantiations; 00056). The table is append-only (the
-- 00056 trigger): the provenance fact "project X was created from template
-- Y version Z" never changes and never disappears. These queries INSERT
-- and SELECT only.

-- name: InsertTemplateInstantiation :one
INSERT INTO project_template_instantiations
    (project_id, template_id, template_version, template_name, created_by)
VALUES
    (@project_id, @template_id, @template_version, @template_name, @created_by)
RETURNING *;

-- name: GetTemplateInstantiationByProject :one
-- The project's template origin — at most one row exists (UNIQUE
-- (project_id)); the query answers no-row when the project was created
-- without a template.
SELECT * FROM project_template_instantiations
WHERE project_id = @project_id;
