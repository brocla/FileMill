# FileMill Project Definition

## Overview

FileMill is a local-first file transformation platform.

The purpose of FileMill is to provide a simple interface where a user can submit a file, have it processed by a specialized transformer, and receive the transformed result.

The first user interface will be email:

1. A user sends an email with an attachment.
2. FileMill receives the email.
3. FileMill determines which transformation is requested.
4. FileMill sends the file to the appropriate transformer.
5. The transformer creates the output artifact.
6. FileMill sends the result back to the sender by email.

The user experience should feel like a simple service:

"Send me your file. I will send back the finished product."

---

# Core Architecture Decision

FileMill is a **harness/orchestrator**, not a transformation library.

FileMill should NOT contain code for PDF processing, OCR, image manipulation, document conversion, etc.

Instead, FileMill manages workflow and invokes independent transformer programs.

A transformer can be written in any language:

* Go
* Python
* C#
* Rust
* shell scripts
* future languages

The only requirement is that it follows the FileMill transformer contract.

The architecture should allow new capabilities to be added by creating new transformers without modifying the FileMill core.

---

# Technology Decisions

## Core Language

FileMill will be implemented in Go.

Reasons:

* excellent fit for long-running services
* strong concurrency model
* simple deployment
* excellent process management
* produces a single executable
* good fit for orchestration software

Development environment:

* Windows 11 development machine
* AI-assisted development using Codex
* Git-based source control

---

# Design Philosophy

Follow these principles:

## 1. Unix philosophy

Do one thing well.

FileMill manages:

* receiving requests
* routing work
* launching transformers
* tracking jobs
* returning results

Transformers do one thing well.

---

## 2. Loose coupling

The harness should not know how a transformation works.

It only knows:

Input artifact(s)
|
Transformer
|
Output artifact(s)

---

## 3. Everything is a job

The central concept is a Job.

A job represents:

* incoming request
* input files
* selected transformer
* options
* execution state
* output files
* result message

The system should be designed around jobs, not emails.

Email is only one input/output adapter.

Future adapters may include:

* CLI
* web API
* scheduled jobs
* desktop application

---

# Transformer Contract

Transformers are independent executable programs.

A transformer receives a JSON job description and produces output files plus a JSON result.

Example invocation:

```
transformer.exe job.json
```

The transformer receives a working directory containing:

```
job/
 |
 +-- job.json
 |
 +-- input/
 |      input files
 |
 +-- output/
        destination for results
```

The transformer must:

1. Read the job.json file.
2. Read input files from the input directory.
3. Write output files to the output directory.
4. Create result.json.
5. Exit with an appropriate exit code.

---

# Transformer Result Contract

Successful result example:

```json
{
  "success": true,
  "message": "File transformed successfully",
  "output_files": [
    {
      "name": "result.pdf",
      "path": "output/result.pdf"
    }
  ],
  "details": {}
}
```

Failure example:

```json
{
  "success": false,
  "message": "Unable to process file",
  "error_code": "INVALID_INPUT",
  "details": {}
}
```

The transformer should always produce a result message, even on failure.

---

# Transformer Requirements

Every transformer must:

* be stateless
* never modify input files
* write outputs only to the assigned output directory
* communicate through files and JSON
* provide meaningful error messages
* return a proper exit code
* write diagnostic information to stdout/stderr

The transformer should not know:

* who submitted the file
* how the request arrived
* how the response is delivered

---

# FileMill Components

Initial architecture:

```
                 Email
                   |
                   v
            Email Receiver
                   |
                   v
              Job Manager
                   |
                   v
          Transformer Dispatcher
                   |
          +--------+--------+
          |        |        |
          v        v        v

       PDF      Image      OCR
       Mill     Mill       Mill

          |
          v

          Result
             |
             v

          Email Reply
```

---

# Initial Components

Build in this order:

## Phase 1: Core job engine

Create:

* project structure
* job model
* transformer interface
* transformer runner
* SQLite job database
* logging

Before adding email, prove that FileMill can execute a transformer.

---

## Phase 2: CLI interface

Create a command-line interface:

Example:

```
filemill submit pdf-compress document.pdf
```

This allows testing without email.

---

## Phase 3: Email adapter

Add:

* IMAP monitoring
* attachment extraction
* sender identification
* email reply generation

Email should be an adapter, not the core.

---

# Future Possibilities

Potential transformers:

* PDF compression
* OCR
* PDF merge/split
* image resizing
* image conversion
* document conversion
* report generation
* data cleanup
* spreadsheet processing
* AI-assisted document analysis

The long-term vision is a personal transformation factory.

---

# Important Constraints

Do not over-engineer early.

Avoid:

* microservices
* Kubernetes
* cloud dependencies
* complex databases
* unnecessary frameworks

The first version should be a reliable local application.

The goal is a small, understandable, extensible tool that can grow over time.

---

# Success Criteria

FileMill succeeds when adding a new capability requires only:

1. Create a new transformer executable.
2. Register it with FileMill.
3. Send it a file.

The harness should remain unchanged.
