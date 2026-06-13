-- Add a fourth member role, 'terminal', whose holder may use only the web
-- terminal (hard-blocked from every other API by deny-by-default middleware).
ALTER TABLE member DROP CONSTRAINT IF EXISTS member_role_check;
ALTER TABLE member ADD CONSTRAINT member_role_check
    CHECK (role IN ('owner', 'admin', 'member', 'terminal'));

-- Make the role assignable at invite time.
ALTER TABLE workspace_invitation DROP CONSTRAINT IF EXISTS workspace_invitation_role_check;
ALTER TABLE workspace_invitation ADD CONSTRAINT workspace_invitation_role_check
    CHECK (role IN ('admin', 'member', 'terminal'));
