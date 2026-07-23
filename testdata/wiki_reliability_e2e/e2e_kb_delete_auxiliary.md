# Durable auxiliary-object deletion probe

This document is used only by the local end-to-end reliability suite. It contains an inline image so document parsing must materialize an auxiliary object before multimodal fan-out.

![one-pixel lifecycle probe](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=)

The deletion test intentionally removes the owning knowledge base while processing may still be active. A correct implementation leaves no knowledge row, chunk, embedding, Wiki artifact, pending operation, source file, or auxiliary image behind.
