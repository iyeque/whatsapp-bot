a comprehensive outline of the steps to create a hybrid system with your bot,
   integrating the Google Gemini API, and setting up a basic Retrieval Augmented Generation
  (RAG) system for your advise.md and agent.md files, all running on Oracle Cloud Free Tier.

  ---

  Phase 1: Oracle Cloud Infrastructure Setup


   1. Create Oracle Cloud Account & VM Instance:
       * Sign up for Oracle Cloud Free Tier.
       * Create a new Compute Instance (VM) in a region close to you. Prioritize Ampere A1
         Compute if available for better performance on the free tier.
       * Ensure you download the SSH private key for connecting to the VM.
       * Note the Public IP address of your VM.


   2. Connect to VM & Initial Setup:
       * Use SSH to connect to your VM (e.g., ssh -i /path/to/your/private_key.pem 
         ubuntu@<VM_PUBLIC_IP>).
       * Update system packages: sudo apt update && sudo apt upgrade -y.


   3. Install Go:
       * Download the latest stable Go version (e.g., wget 
         https://go.dev/dl/go1.22.4.linux-amd64.tar.gz).
       * Extract to /usr/local: sudo tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz.
       * Set up Go environment variables (add to ~/.profile or ~/.bashrc):


   1         export PATH=$PATH:/usr/local/go/bin
   2         export GOPATH=$HOME/go
   3         export PATH=$PATH:$GOPATH/bin

       * Apply changes: source ~/.profile (or ~/.bashrc).
       * Verify Go installation: go version.


   4. Clone Your Bot Repository:
       * Install Git: sudo apt install git -y.
       * Clone your repository: git clone 
         https://github.com/your-username/whatsapp-gpt-bot.git.
       * Navigate into the cloned directory: cd whatsapp-gpt-bot.

   5. Install Go Dependencies:
       * go mod tidy (this will download all required modules).

  ---

  Phase 2: Google Gemini API Integration (Code Modifications)


   1. Obtain Google Gemini API Key:
       * Go to Google AI Studio (https://aistudio.google.com/).
       * Sign in and create a new API key. Keep it secure.


   2. Update `whatsapp/bot.go` for Gemini API:
       * Change LLM Endpoint & Response Struct:
           * Replace LM_STUDIO_URL with the Gemini API endpoint (e.g., https://generativelan
             guage.googleapis.com/v1beta/models/gemini-pro:generateContent?key=).
           * Redefine the LMResponse struct to GeminiResponse to match Gemini's JSON
             response structure.
       * Modify `makeAIRequest` Function:
           * Construct the request body in the format expected by the Gemini API (e.g.,
             {"contents": [{"parts": [{"text": "your prompt here"}]}]}).
           * Append your Gemini API key to the GEMINI_API_URL when making the HTTP request.
           * Parse the GeminiResponse to extract the generated text.
       * Secure API Key Handling:
           * Modify your code to read the API key from an environment variable (e.g.,
             os.Getenv("GEMINI_API_KEY")).

  ---

  Phase 3: Retrieval Augmented Generation (RAG) System


   1. Choose Embedding Model:
       * The Gemini API itself offers embedding models (e.g., embedding-001). You'll use
         this to convert text into numerical vectors.


   2. Choose a Vector Store:
       * For simplicity and free tier constraints, consider an in-memory vector store or a
         lightweight Go library that provides vector search capabilities. For a small number
          of documents like advise.md and agent.md, this is sufficient.
       * Alternatively, if you anticipate many documents or more complex search, you might
         look into client libraries for services like ChromaDB (can be self-hosted or
         cloud-managed, but self-hosting might exceed free tier).


   3. Implement RAG Logic (New Functions/Modifications):
       * `embedDocument(text string) []float32`: A function that takes text, sends it to the
          Gemini embedding API, and returns its embedding vector.
       * `storeEmbeddings(documents map[string]string)`: A function that reads advise.md and
          agent.md, embeds their content (or chunks of their content), and stores them in
         your chosen vector store along with their original text. This would typically run
         once at bot startup.
       * `retrieveContext(query string) string`: A function that:
           * Embeds the user's query.
           * Searches the vector store for the most similar document embeddings.
           * Returns the relevant text snippets from advise.md and agent.md.
       * Integrate into `handleTextMessage` (or `makeAIRequest`):
           * Before calling makeAIRequest, call retrieveContext with the user's message.
           * Prepend the retrieved context to the user's message before sending it to the
             Gemini LLM. Example: fmt.Sprintf("Context: %s\n\nUser: %s", retrievedContext, 
             userMsg).

  ---

  Phase 4: Deployment and Continuous Operation on Oracle Cloud


   1. Build Your Go Application:
       * On your VM, in the whatsapp-gpt-bot directory: go build -o whatsapp-bot . (This
         creates an executable named whatsapp-bot).

   2. Configure `systemd` Service:
       * Create a systemd service file (e.g., /etc/systemd/system/whatsapp-bot.service):


    1         [Unit]
    2         Description=WhatsApp GPT Bot
    3         After=network.target
    4 
    5         [Service]
    6         User=ubuntu # Or your VM's user
    7         WorkingDirectory=/home/ubuntu/whatsapp-gpt-bot # Your bot's directory
    8         Environment="GEMINI_API_KEY=YOUR_API_KEY_HERE" # Set your API key 
      here
    9         ExecStart=/home/ubuntu/whatsapp-gpt-bot/whatsapp-bot # Path to your 
      executable
   10         Restart=always
   11         RestartSec=5
   12 
   13         [Install]
   14         WantedBy=multi-user.target

       * Important: Replace YOUR_API_KEY_HERE with your actual Gemini API key.
       * Reload systemd daemon: sudo systemctl daemon-reload.
       * Enable the service to start on boot: sudo systemctl enable whatsapp-bot.service.
       * Start the service: sudo systemctl start whatsapp-bot.service.
       * Check status: sudo systemctl status whatsapp-bot.service.
       * View logs: sudo journalctl -u whatsapp-bot.service -f.


   3. Firewall Rules (Oracle Cloud Security List):
       * Ensure your Oracle Cloud VM's Security List (VCN -> Subnets -> Security Lists)
         allows outbound connections on HTTPS (port 443) to reach the Gemini API. Inbound
         rules are typically not needed for the bot itself unless you expose its dashboard.

  ---


  Phase 5: Hybrid System (Active-Passive Failover Strategy)

   1. Understand WhatsApp Limitation:
       * Only one instance of your bot can be actively connected to a single WhatsApp
         account at any given time. Running both simultaneously will cause disconnections
         and issues.


   2. Primary (Active) - Oracle Cloud:
       * The systemd service on your Oracle Cloud VM will ensure your bot is running 24/7
         and is the primary connection to WhatsApp.


   3. Backup (Passive) - Local Machine:
       * Keep your local bot code updated (e.g., git pull).
       * Do NOT run your local bot unless the Oracle Cloud instance is down.
       * Ensure your local environment variables (e.g., GEMINI_API_KEY) are set up correctly
          for a quick manual failover.


   4. Manual Failover Procedure:
       * If Oracle Cloud Bot Fails:
           1. SSH into your Oracle Cloud VM and stop the systemd service: sudo systemctl stop
               whatsapp-bot.service.
           2. On your local machine, navigate to your bot's directory and run: go run 
              main.go.
       * When Oracle Cloud Bot is Restored:
           1. Stop the local bot (Ctrl+C in your terminal).
           2. SSH into your Oracle Cloud VM and start the systemd service: sudo systemctl 
              start whatsapp-bot.service.

  ---


  This outline provides a complete roadmap. We can now proceed with implementing the code
  changes for Gemini API integration.