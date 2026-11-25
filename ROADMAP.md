# Features & roadmap
* Matrix → Teams
  * [x] Message content
    * [x] Plain text
    * [x] Formatted messages
    * [x] Mentions
    * [x] Media/files (images inline, other files through OneDrive)
    * [x] Replies
  * [x] Message edits
  * [x] Message redactions
  * [x] Reactions
  * [ ] Typing notifications (Graph has no API for them)
  * [x] Read receipts (only at the newest message)
  * [ ] Room metadata changes
  * [ ] Membership actions
* Teams → Matrix
  * [x] Message content
    * [x] Plain text
    * [x] Formatted messages
    * [x] Mentions
    * [x] Inline images
    * [x] Shared files
    * [x] Replies
    * [ ] Adaptive cards (shown as a placeholder)
  * [x] Message edits
  * [x] Message deletions
  * [x] Reactions
  * [x] Call and meeting log (who started it and how long it took)
  * [ ] Typing notifications
  * [ ] Read receipts
  * [ ] Presence
  * [x] Chat types
    * [x] One-to-one chats
    * [x] Group chats
    * [x] Meeting chats
    * [ ] Team channels
  * [x] User metadata (name, photo)
  * [ ] Chat metadata changes after the first sync
  * [ ] Backfill beyond the first messages of a chat
* Misc
  * [x] Sign-in without an own app registration
  * [x] Token refresh
  * [x] Directory search and starting one-to-one chats
  * [ ] Starting group chats
  * [ ] Change notifications instead of polling (needs a registered app with a public endpoint)
