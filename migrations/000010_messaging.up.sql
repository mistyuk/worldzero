-- M3 — agents talk to each other.
--
-- VISION §9 makes this a first-class world primitive rather than a feature: a
-- civilisation is agents coordinating, and coordination is communication.
--
-- INVARIANT #6 GOVERNS THIS TABLE MORE THAN ANY OTHER. Message bodies are
-- agent-generated text, stored verbatim and treated as DATA, never as
-- instructions. Nothing in the engine parses them, branches on them, or grants
-- authority because of them. When Phase 2 brings LLM agents that read each
-- other's messages, prompt injection becomes their runner's problem — but it is
-- our job never to have built a path where text becomes authority.
CREATE TABLE messages (
    id           text PRIMARY KEY CHECK (id ~ '^msg_[0-9A-HJKMNP-TV-Z]{26}$'),
    from_agent_id text NOT NULL REFERENCES agents (id),

    -- Exactly one of these. A direct message has a recipient; something said
    -- aloud has a room. Both would be ambiguous about who may read it, and
    -- neither would be a message at all.
    to_agent_id  text REFERENCES agents (id),
    location_id  text REFERENCES locations (id),

    body         text NOT NULL,
    created_at   timestamptz NOT NULL,  -- W

    -- Read receipts are on the DM only. Something said in a room is not
    -- addressed to anyone, so "unread" is meaningless for it.
    read_at      timestamptz,           -- W

    CONSTRAINT messages_exactly_one_audience CHECK (
        (to_agent_id IS NOT NULL AND location_id IS NULL)
        OR (to_agent_id IS NULL AND location_id IS NOT NULL)
    ),
    CONSTRAINT messages_read_only_dms CHECK (
        read_at IS NULL OR to_agent_id IS NOT NULL
    )
);

-- The inbox: newest first, cursor-paginated, unread counted.
CREATE INDEX messages_inbox_idx ON messages (to_agent_id, id DESC)
    WHERE to_agent_id IS NOT NULL;

CREATE INDEX messages_unread_idx ON messages (to_agent_id)
    WHERE to_agent_id IS NOT NULL AND read_at IS NULL;

-- What was said in a room, for anyone standing in it.
CREATE INDEX messages_room_idx ON messages (location_id, id DESC)
    WHERE location_id IS NOT NULL;

-- Sent messages, so an agent can see its own side of a conversation.
CREATE INDEX messages_sent_idx ON messages (from_agent_id, id DESC);
