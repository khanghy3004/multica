ALTER TABLE workspace_invitation DROP CONSTRAINT IF EXISTS workspace_invitation_role_check;
ALTER TABLE workspace_invitation ADD CONSTRAINT workspace_invitation_role_check
    CHECK (role IN ('admin', 'member'));

ALTER TABLE member DROP CONSTRAINT IF EXISTS member_role_check;
ALTER TABLE member ADD CONSTRAINT member_role_check
    CHECK (role IN ('owner', 'admin', 'member'));
