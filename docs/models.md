# Models

bgm never bundles model weights. `bgm setup` downloads them from their
publishers into bgm's data directory, with visible progress, and the
default path requires no account, token, or license click-through.

## ACE-Step 1.5 (default engine)

- Code: [github.com/ace-step/ACE-Step-1.5](https://github.com/ace-step/ACE-Step-1.5),
  MIT license, pinned to a release tag by `bgm setup`.
- Weights: [huggingface.co/ACE-Step/Ace-Step1.5](https://huggingface.co/ACE-Step/Ace-Step1.5),
  MIT license, ungated, about 10 GB. The bundle contains the turbo
  diffusion model, a 1.7B planner language model, a text embedder and the
  audio VAE.
- On GPUs with less memory the engine automatically selects the smaller
  0.6B planner LM from
  [huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B](https://huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B)
  (also MIT, ungated, about 1.2 GB), downloading it on first start.
- The model card states that music generated with ACE-Step 1.5 may be
  used commercially (the training data is licensed, royalty-free, or
  synthetic). Verify current terms on the model card if this matters for
  your use.
- Output: 48 kHz stereo, full-song generations with optional sung vocals;
  instrumental tracks are requested with the engine's `[Instrumental]`
  lyrics convention.

The engine runs as a local API server child process that bgm starts,
supervises and restarts as needed. It listens on localhost only.

## Ollama models (optional)

If you run [Ollama](https://ollama.com), bgm can use whatever model you
have installed to polish prompts and write lyrics. bgm does not download
Ollama models; whichever model you point it at keeps its own license
terms. Without Ollama, bgm uses a built-in deterministic path and the
engine's own planner for lyrics, so this integration is a bonus, not a
requirement.

## Noise synthesis (built in)

White, pink and brown noise are synthesized directly by bgm in pure Go
(no model, no GPU): pink via Paul Kellet's filter, brown via a leaky
integrator. Noise serves as the instant-start bed, the last-resort
fallback when the engine is unavailable, and a first-class mode for
sleep/masking via the `pink-noise` preset or steering ("generate brown
noise").
