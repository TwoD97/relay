# Move Mastermind to another machine

Use **Mastermind → Overview → Move Mastermind** to keep the coordinator and its
control service on an always-on Linux machine. This moves the control service,
not merely the terminal viewer. The hosted controller keeps its inbox, tasks,
pages, lessons and connections to other machines after the source desktop closes.
It remains dependent on the destination machine staying powered on and online.

## Prepare and move

1. Connect the destination in Relay. Install and sign in to the same coding
   harness there, and configure its SSH access to the other machines it must
   manage. Relay does not copy provider credentials or SSH keys.
2. Finish or review any inbox instruction still marked processing. Save useful
   decisions, next steps and context in Mastermind's tasks, pages or lessons.
3. Choose **Move Mastermind**, select the destination, then **Check destination**.
   Relay stages a private controller in standby and checks its harness and host
   access. This step starts no second coordinator and leaves other sessions alone.
4. Review the counts, warnings and blockers. Confirm the source coordinator may
   stop, then choose **Move and start Mastermind**.
5. Keep the source desktop open until Relay confirms **Hosted on [machine]**.
   Its Mastermind views now read and write through the hosted controller. You can
   close the desktop; reopening it reconnects to the same hosted authority.

The source runtime must support stopping a session while preserving its record.
Older runtimes show a blocker. Finish that coordinator and update its service
before moving; Relay does not restart a daemon containing other live sessions
as part of the handoff.

## What moves

The saved workspace, pages, tasks, lessons and queued inbox instructions are
transferred. Other remote host endpoints are shared so the destination can use
its own SSH access. A destination-linked task is remapped to that controller's
local runtime. Links to a terminal on the source desktop are removed from the
copied task; the task itself is retained.

The destination starts a **new provider conversation** with the saved handoff.
The previous coordinator's terminal output stays on its original machine.
Provider conversation files, authentication, SSH keys, project files, active
shell processes, and messaging credentials are not transferred. Other agents'
existing runtime sessions stay where they are and are not restarted.

After moving, configure Telegram, Discord or WhatsApp through Mastermind's
remote-control settings. Those settings now target the destination controller.
Source connectors pause rather than continuing to consume messages or approvals.
Personal WhatsApp uses a new explicit linked-device setup on that controller.

## Interrupted handoffs

Relay saves an operation identity before dispatch, freezes source control writes,
stages the destination snapshot, verifies the exact source session has stopped,
and activates the destination once. Only an exact destination receipt confirms
success. It does not infer ownership from a title or PID alone.

If the response is lost, Relay checks the saved receipt without repeating a stop,
seed, launch or approval. An uncertain outcome stays visible. Keep both machines
available and inspect the migration state; do not start a replacement coordinator
or delete the private state files to make the warning disappear. A failure before
the source freeze can be reviewed and prepared again. A failure after that fence
requires inspection of the exact operation and destination receipt; automatic
rollback could create two active controllers and is deliberately not attempted.

## Current boundaries

This release supports the first handoff from a desktop-owned controller to a
saved Linux host. Reopening that same desktop resumes its saved hosted binding.
Moving the hosted coordinator again, moving it back, and adopting its authority
from a different desktop do not yet have a complete user-facing workflow.

Peer workspace import is separate: it discovers and copies snapshots without
launching a coordinator or transferring ownership. It must not be treated as an
automatic takeover of a hosted Mastermind. Provider-native conversation history
also remains tied to its original machine and harness.
