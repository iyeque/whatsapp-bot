# Advise: Architectural Improvements for Maximus

The following areas have been identified for technical refinement.

### 1. AI and Feature Enhancements

*   **Voice-Based Conversations (The Full Loop):**
    *   **Why:** Transform the bot into a voice-first assistant. This allows users to interact naturally by sending and receiving voice messages, creating a conversational experience.
    *   **Workflow:** `User Voice Message -> Download Audio -> STT (Whisper) -> Transcribed Text -> LLM -> Text Response -> TTS (Coqui/ElevenLabs) -> Generated Audio -> Send Voice Message`.

*   **Multi-modal AI (Image Understanding):**
    *   **Why:** Allow users to ask questions about images they send.
    *   **How:** Modify `handleImageMessage` to convert images to base64 and send them to a multi-modal AI endpoint.

*   **AI Tool Use / Function Calling:**
    *   **Why:** Give the AI the ability to perform actions (e.g., weather, search, calendar).
    *   **How:** Implement a function-calling framework to execute tools and send the results back to the LLM.

### 2. Performance and Scalability

*   **Persistent Caching:** (Ongoing) Transition from in-memory to persistent storage (e.g., Redis).
*   **Optimized Database Access:** Ensure efficient database connection pooling.

### 3. Robustness and Reliability

*   **Dead-Letter Queue (DLQ):** Implement a table for failed message inspection.
*   **Health Check Endpoint:** Add a `/health` endpoint for monitoring infrastructure.

### 4. Code Quality and Maintainability (Recent Updates)

*   **Unit and Integration Testing:** *Critical.* Currently, the project lacks automated tests.
*   **Structured Logging:** (Complete: Configured `zerolog` for console and file output).
*   **Persistent Conversation Memory:** (Complete: SQLite-based storage for messages and summaries).
*   **Memory Stabilization:** (Complete: Implemented Top-K retrieval and character-based context budgeting).
*   **Dependency Injection:** Refactor `NewBot` to accept dependencies to improve isolation and testability.
