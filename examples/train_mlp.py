"""Train a tiny neural network on synthetic XOR data with Relay.

Run it through the CLI:

    relay run examples/train_mlp.py::train --epochs 100

Or run this file directly; the ``__main__`` block calls ``train.remote()``:

    python examples/train_mlp.py

The example intentionally trains on CPU so it works in zero-config local
Docker mode and on any machine in a Relay fleet. The data is generated inside
the job, so no dataset download or persistent volume is needed.
"""

from __future__ import annotations

import relay


app = relay.App("tiny-mlp")

# Image.python() uses the same Python minor as the Relay client. Dependencies
# are installed once when this content-addressed image is first used.
torch_image = relay.Image.python().pip("torch")


@app.function(
    image=torch_image,
    cpu=2,
    memory="2GB",
    timeout="10m",
)
def train(
    epochs: int = 100,
    learning_rate: float = 0.03,
    seed: int = 7,
) -> dict[str, object]:
    """Train a small multilayer perceptron and return its final metrics."""
    if epochs < 1:
        raise ValueError("epochs must be at least 1")
    if learning_rate <= 0:
        raise ValueError("learning_rate must be positive")

    # Import job dependencies inside the function. The client only needs the
    # Relay SDK; torch is supplied by torch_image on the execution machine.
    import torch
    from torch import nn

    torch.manual_seed(seed)
    torch.set_num_threads(2)

    # XOR is not linearly separable, so the hidden layers have real work to do.
    generator = torch.Generator().manual_seed(seed)
    features = torch.rand((2_048, 2), generator=generator) * 2 - 1
    labels = (features[:, 0] * features[:, 1] > 0).long()

    order = torch.randperm(len(features), generator=generator)
    train_indices = order[:1_600]
    test_indices = order[1_600:]
    train_x, train_y = features[train_indices], labels[train_indices]
    test_x, test_y = features[test_indices], labels[test_indices]

    model = nn.Sequential(
        nn.Linear(2, 16),
        nn.ReLU(),
        nn.Linear(16, 16),
        nn.ReLU(),
        nn.Linear(16, 2),
    )
    optimizer = torch.optim.Adam(model.parameters(), lr=learning_rate)
    loss_fn = nn.CrossEntropyLoss()

    report_every = max(1, epochs // 5)
    final_loss = 0.0
    for epoch in range(1, epochs + 1):
        model.train()
        optimizer.zero_grad()
        loss = loss_fn(model(train_x), train_y)
        loss.backward()
        optimizer.step()
        final_loss = loss.item()

        if epoch == 1 or epoch % report_every == 0 or epoch == epochs:
            print(f"epoch={epoch:>3}/{epochs} loss={final_loss:.4f}", flush=True)

    model.eval()
    with torch.no_grad():
        predictions = model(test_x).argmax(dim=1)
        accuracy = (predictions == test_y).float().mean().item()

    metrics = {
        "epochs": epochs,
        "loss": round(final_loss, 4),
        "test_accuracy": round(accuracy, 4),
        "train_samples": len(train_x),
        "test_samples": len(test_x),
    }
    print(f"finished: {metrics}", flush=True)
    return metrics


if __name__ == "__main__":
    print(train.remote())
