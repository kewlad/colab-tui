import time
import sys

print("=== Training Simulation ===")
print(f"Python {sys.version}")
print()

for epoch in range(1, 6):
    print(f"Epoch {epoch}/5 - training...", flush=True)
    time.sleep(1)
    loss = 1.0 / epoch
    acc = 1 - loss / 2
    print(f"  loss: {loss:.4f}  acc: {acc:.4f}", flush=True)

print()
print("Training complete!")
