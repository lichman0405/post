# Worker recovery drill

This directory is the controlled stop/respawn sample for WEB-28 (T1334).
The Worker writes a minimal Go test here; the Supervisor stops the first
attempt mid-run and respawns it from disk state. It is not product code.
