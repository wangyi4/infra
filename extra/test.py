#! /usr/bin/env python3

import os
import e2b

key=os.environ.get("E2B_API_KEY")
url=os.environ.get("E2B_API_URL")
sandbox_url=os.environ.get("E2B_SANDBOX_URL")

print("key: ", key)
print("url: ", url)
print("sandbox_url: ", sandbox_url)

# Configure for local development
sandbox = e2b.Sandbox.create(
    template="base",
    api_key=os.environ.get("E2B_API_KEY"),
    api_url=os.environ.get("E2B_API_URL", "http://localhost:3000"),
    sandbox_url=os.environ.get("E2B_SANDBOX_URL", "http://localhost:3002"),
)
print(sandbox.sandbox_id)  # sandbox is running locally
