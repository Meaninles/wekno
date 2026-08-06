# Citation output live E2E

This suite exercises the running local production-like topology through its
public API. The wrapper reads the encrypted tenant key from the local database,
decrypts it only in memory, and never prints or writes the plaintext key.

Run all covered agent cases:

```powershell
python custom/tests/citation_output_e2e/run_with_local_tenant_key.py
```

Run selected cases:

```powershell
python custom/tests/citation_output_e2e/run_with_local_tenant_key.py --case table --case custom-rag
```
