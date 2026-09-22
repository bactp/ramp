The video component's process body is kept inline in the AppBundle template so the
whole application is one declarative object. It is reproduced here for readability.

State split (this is the whole point of Scenario 1):

  in-memory ONLY, recoverable only by container checkpoint:
      frames_decoded   - decoder work done since the session started
      session_uuid     - identity of this decode session
      ring[]           - recent frame checksums

  in Redis, recoverable only by Redis replication:
      ramp:video:position  - committed stream position
      ramp:video:session   - session id bound to that position

  invariant tying them together (checked at VALIDATE and after recovery):
      frames_decoded == position * FRAMES_PER_TICK
