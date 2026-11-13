---
name: scheduling
author: Morde
description: Plan meeting times from authorized calendar availability and keep unconfirmed proposals provisional.
version: "1"
---

# Scheduling

Use calendar tools only when those exact tools are listed for this Task and authorized for the named account. A skill does not grant calendar access. Without authorized availability, use only times the user supplied and label every option provisional; never claim another participant is free.

## Find workable times

- Establish the meeting purpose, required attendees, duration, date range, each known time zone, and hard constraints. Ask together for essential details that are missing.
- Read availability only from authorized calendars in scope. Treat tentative events and holds as conflicts; say which calendars or participants could not be checked.
- Offer two or three options with the date, local time, time-zone name, and UTC offset for each participant when known. Check daylight-saving changes for the requested date and include travel or transition buffers when relevant.
- If any participant's availability is unknown, label the proposed time provisional until they confirm.

## Create or change events

- Suggest times in chat unless the user asks to create or update a calendar event. Do not treat a request to find a time as permission to book, move, cancel, or invite.
- For an authorized calendar write, use only the matching tool listed for this Task and obey its approval step. Before committing, make the title, time zone, attendees, and notification effect clear to the user.
- Read the event back after a confirmed write. If the result is uncertain, check the same event or operation before retrying; do not create a duplicate to test whether the first write succeeded.
- If the user asks for a local plan, use `morde_workspace_list`, `morde_workspace_search`, `morde_workspace_read`, and `morde_workspace_write` only in the Workspace authorized for this Task, then read back the committed revision when available.

## Report

Distinguish checked availability from user-provided constraints and unconfirmed proposals. State the time zone used and report whether an event was proposed, saved, or confirmed by the calendar service.
