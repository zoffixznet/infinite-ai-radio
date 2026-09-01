package acestep

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The engine checkout is pinned to a release tag, but several of its
// defects and gaps hit this application hard enough that waiting for an
// upstream release is not an option. Each fix below is a minimal source
// edit applied to the checkout by setup and by the engine daemon at
// start (idempotent, cheap when nothing needs doing), and re-applied
// after a tag update, which resets the tree first. A hunk anchors on
// the exact upstream text and refuses to apply when that text has
// drifted, so a future tag bump either carries the fix cleanly or
// fails loudly instead of silently shipping without it.

// patchHunk replaces one uniquely-occurring stretch of source text in
// one file. A hunk whose full replacement text is already present is
// done; otherwise its find text must occur exactly once.
type patchHunk struct {
	file    string
	find    string
	replace string
}

// enginePatch is one named fix, possibly spanning several files.
type enginePatch struct {
	name  string
	hunks []patchHunk
}

var enginePatches = []enginePatch{
	{
		// The planner LM's two custom decoding loops read only the
		// final position's logits, but their shared prefill forward
		// pass materializes logits for every prompt position: a
		// [batch, prompt_len, 217k-vocab] bf16 tensor of 0.5-1.8 GiB,
		// allocated and discarded unread on every lyric-planning call.
		// On a card shared with other tenants that transient is what
		// runs out of memory. Keeping only the last position is
		// numerically identical.
		name: "lm-prefill-logits",
		hunks: []patchHunk{{
			file: "acestep/llm_inference.py",
			find: `        if past_key_values is None:
            outputs = model(
                input_ids=generated_ids,
                **model_kwargs,
                use_cache=use_cache,
            )
`,
			replace: `        if past_key_values is None:
            # iar-patch: lm-prefill-logits. Both generation loops read
            # only logits[:, -1, :], but this prefill materialized
            # logits for every prompt position - a huge tensor
            # allocated and thrown away unread. Keep the final
            # position only; the output is numerically identical.
            outputs = model(
                input_ids=generated_ids,
                **model_kwargs,
                use_cache=use_cache,
                logits_to_keep=1,
            )
`,
		}},
	},
	{
		// In sample mode the planner LM chooses the track length and
		// the request's audio_duration is discarded, so nothing bounds
		// how long (and how memory- and time-expensive) a track can
		// get. When the supervisor sets ACESTEP_SAMPLE_DURATION_CAP,
		// treat it as a ceiling on the LM's choice; shorter choices
		// pass through untouched, so track lengths stay varied.
		name: "sample-duration-cap",
		hunks: []patchHunk{
			{
				file:    "acestep/api/llm_generation_inputs.py",
				find:    "from dataclasses import dataclass\n",
				replace: "import os\n\nfrom dataclasses import dataclass\n",
			},
			{
				file: "acestep/api/llm_generation_inputs.py",
				find: "        audio_duration = sample_result.duration\n",
				replace: `        audio_duration = sample_result.duration
        # iar-patch: sample-duration-cap. A missing or unparseable LM
        # duration becomes the cap rather than passing through:
        # downstream it would turn into the auto sentinel and the codes
        # phase would run bounded only by the model ceiling (~600s),
        # which is the runaway this cap exists to prevent.
        _iar_cap_raw = os.getenv("ACESTEP_SAMPLE_DURATION_CAP", "")
        if _iar_cap_raw:
            try:
                _iar_cap = float(_iar_cap_raw)
            except ValueError:
                _iar_cap = 0.0
            if _iar_cap > 0:
                try:
                    if audio_duration is None or float(audio_duration) > _iar_cap:
                        audio_duration = _iar_cap
                except (TypeError, ValueError):
                    audio_duration = _iar_cap
`,
			},
		},
	},
	{
		// When a request carries lyrics but no duration, the planner's
		// CoT reads the lyrics and its planned duration becomes the
		// codes phase's target - the "length follows the song" path.
		// This bounds that plan with the same ceiling the supervisor
		// sets for sample mode, so one confused plan cannot run to the
		// model's own ~600s limit.
		name: "auto-duration-cap",
		hunks: []patchHunk{{
			file: "acestep/llm_inference.py",
			find: `                cot_duration = float(metadata["duration"])
`,
			replace: `                cot_duration = float(metadata["duration"])
                # iar-patch: auto-duration-cap (see the supervisor's
                # patch list for rationale).
                _iar_cap_raw = os.getenv("ACESTEP_SAMPLE_DURATION_CAP", "")
                if _iar_cap_raw:
                    try:
                        _iar_cap = float(_iar_cap_raw)
                    except ValueError:
                        _iar_cap = 0.0
                    if _iar_cap > 0 and cot_duration > _iar_cap:
                        cot_duration = _iar_cap
`,
		}},
	},
	{
		// A planning job runs the LM phases (metadata, lyrics, audio
		// codes) and returns them without ever touching the diffusion
		// model, so a supervisor can batch all planning while the DiT
		// is off the card, then render each plan later from its codes
		// (audio_code_string with thinking off - that half is stock).
		name: "plan-only",
		hunks: []patchHunk{
			{
				file: "acestep/api/http/release_task_models.py",
				find: `    audio_code_string: str = Field(
        default="",
        description="User-provided audio semantic codes string for code-control generation. When non-empty, skips LM code generation.",
    )
`,
				replace: `    audio_code_string: str = Field(
        default="",
        description="User-provided audio semantic codes string for code-control generation. When non-empty, skips LM code generation.",
    )
    # iar-patch: plan-only. Run the LM phases (metadata, lyrics, audio
    # codes) and return them without touching the diffusion model, so a
    # supervisor can batch all planning while the DiT is off the card.
    plan_only: bool = Field(
        default=False,
        description="Run LM planning (metadata/lyrics/audio codes) and return without diffusion.",
    )
`,
			},
			{
				file:    "acestep/api/http/release_task_request_builder.py",
				find:    "        use_format=parser.bool(\"use_format\"),\n",
				replace: "        use_format=parser.bool(\"use_format\"),\n        plan_only=parser.bool(\"plan_only\"),\n",
			},
			{
				file: "acestep/inference.py",
				find: `    # 5Hz Language Model Parameters
    thinking: bool = True
`,
				replace: `    # 5Hz Language Model Parameters
    thinking: bool = True
    # iar-patch: plan-only (see release_task_models).
    plan_only: bool = False
`,
			},
			{
				file:    "acestep/api/job_generation_setup.py",
				find:    "        audio_codes=req.audio_code_string if req.audio_code_string else \"\",\n",
				replace: "        audio_codes=req.audio_code_string if req.audio_code_string else \"\",\n        plan_only=bool(getattr(req, \"plan_only\", False)),\n",
			},
			{
				file: "acestep/inference.py",
				find: `        # Repaint/cover/extract: no LM run, so conditioning must come from params (caption + lyrics from GUI).
`,
				replace: `        # iar-patch: plan-only. Everything the DiT would need is now in
        # hand (final metadata, lyrics, audio codes); a planning job
        # stops here so the diffusion model is never touched.
        if getattr(params, "plan_only", False):
            plan_codes = audio_code_string_to_use
            if isinstance(plan_codes, list):
                plan_codes = plan_codes[0] if plan_codes else ""
            return GenerationResult(
                audios=[],
                status_message="Plan complete (no diffusion)",
                extra_outputs={
                    "lm_metadata": lm_generated_metadata or {},
                    "plan_audio_codes": plan_codes or "",
                    "time_costs": lm_total_time_costs,
                },
                success=True,
                error=None,
            )

        # Repaint/cover/extract: no LM run, so conditioning must come from params (caption + lyrics from GUI).
`,
			},
			{
				file: "acestep/api/job_result_payload.py",
				find: `        "generation_info": generation_info,
        "status_message": result.status_message,
`,
				replace: `        "generation_info": generation_info,
        "status_message": result.status_message,
        # iar-patch: plan-only: the planned audio codes ride the payload.
        "audio_codes": result.extra_outputs.get("plan_audio_codes", ""),
`,
			},
			{
				file: "acestep/api/jobs/local_cache_updates.py",
				find: `            else:
                result_data = [{
                    "file": "",
                    "wave": "",
                    "status": status_int,
                    "create_time": int(create_time),
                    "env": env,
                    "prompt": final_prompt,
                    "lyrics": final_lyrics,
                    "metas": metas,
                    "generation_info": generation_info,
                    "seed_value": seed_value,
                    "lm_model": lm_model,
                    "dit_model": dit_model,
                    "progress": 1.0,
                    "stage": "succeeded",
                }]
`,
				replace: `            else:
                result_data = [{
                    "file": "",
                    "wave": "",
                    "status": status_int,
                    "create_time": int(create_time),
                    "env": env,
                    "prompt": final_prompt,
                    "lyrics": final_lyrics,
                    "metas": metas,
                    "generation_info": generation_info,
                    "seed_value": seed_value,
                    "lm_model": lm_model,
                    "dit_model": dit_model,
                    # iar-patch: plan-only: a planning job has no audio;
                    # its payload is the codes for a later render job.
                    "audio_codes": result.get("audio_codes", ""),
                    "progress": 1.0,
                    "stage": "succeeded",
                }]
`,
			},
			{
				file: "acestep/api/http/query_result_service.py",
				find: `            ] if audio_paths else [{
                "file": "",
                "wave": "",
                "status": status_int,
                "create_time": int(create_time),
                "env": env,
                "prompt": metas.get("caption", ""),
                "lyrics": metas.get("lyrics", ""),
`,
				replace: `            ] if audio_paths else [{
                "file": "",
                "wave": "",
                "status": status_int,
                "create_time": int(create_time),
                "env": env,
                "prompt": metas.get("caption", ""),
                "lyrics": metas.get("lyrics", ""),
                # iar-patch: plan-only: a planning job has no audio, its
                # payload is the codes and metadata for a later render.
                "audio_codes": record.result.get("audio_codes", ""),
`,
			},
		},
	},
	{
		// With ACESTEP_OFFLOAD_DIT_TO_DISK the DiT is never parked in
		// system memory: startup loads only its config and silence
		// latent, the weights stream from disk straight onto the GPU
		// inside the model context, stay resident across a render
		// batch, and are dropped entirely when planning work starts or
		// the process exits. System memory is never the parking lot.
		name: "dit-from-disk",
		hunks: []patchHunk{
			{
				file: "acestep/core/generation/handler/init_service_loader.py",
				find: `        if not os.path.exists(model_checkpoint_path):
            raise FileNotFoundError(f"ACE-Step V1.5 checkpoint not found at {model_checkpoint_path}")
`,
				replace: `        if not os.path.exists(model_checkpoint_path):
            raise FileNotFoundError(f"ACE-Step V1.5 checkpoint not found at {model_checkpoint_path}")

        # iar-patch: dit-from-disk. With ACESTEP_OFFLOAD_DIT_TO_DISK the
        # DiT is never parked in system memory: startup loads only its
        # config and silence latent, and the weights stream from disk to
        # the GPU inside the model context, then are dropped entirely on
        # eviction. Requires the CPU-offload base flags so the context
        # machinery is active. Compile and quantization are not applied
        # in this mode (the API server never enables either).
        self.offload_dit_to_disk = (
            os.environ.get("ACESTEP_OFFLOAD_DIT_TO_DISK", "").strip().lower() in ("1", "true", "yes")
            and self.offload_to_cpu and self.offload_dit_to_cpu
        )
        self._iar_dit_checkpoint = model_checkpoint_path
        self._iar_use_flash = use_flash_attention
        if self.offload_dit_to_disk:
            from transformers import AutoConfig
            self.model = None
            self.config = AutoConfig.from_pretrained(model_checkpoint_path, trust_remote_code=True)
            self._sync_alignment_config()
            silence_latent_path = os.path.join(model_checkpoint_path, "silence_latent.pt")
            if not os.path.exists(silence_latent_path):
                raise FileNotFoundError(f"Silence latent not found at {silence_latent_path}")
            self.silence_latent = torch.load(silence_latent_path, weights_only=True).transpose(1, 2)
            self.silence_latent = self.silence_latent.to("cpu").to(self.dtype)
            logger.info("[initialize_service] dit-from-disk: DiT load deferred; weights stream from disk on demand")
            return "sdpa"
`,
			},
			{
				file: "acestep/core/generation/handler/init_service_loader.py",
				find: `        silence_latent_device = "cpu" if self.offload_to_cpu and self.offload_dit_to_cpu else device
        self.silence_latent = self.silence_latent.to(silence_latent_device).to(self.dtype)
        return attn_implementation
`,
				replace: `        silence_latent_device = "cpu" if self.offload_to_cpu and self.offload_dit_to_cpu else device
        self.silence_latent = self.silence_latent.to(silence_latent_device).to(self.dtype)
        return attn_implementation

    # iar-patch: dit-from-disk helpers.
    def _iar_materialize_dit(self, device: str) -> None:
        """Stream the DiT from its checkpoint straight onto device."""
        from transformers import AutoModel

        candidates = []
        if self._iar_use_flash and self.is_flash_attention_available(device):
            candidates.append("flash_attention_2")
        candidates += [c for c in ("sdpa", "eager") if c not in candidates]
        last_error = None
        self.model = None
        for candidate in candidates:
            try:
                self.model = AutoModel.from_pretrained(
                    self._iar_dit_checkpoint,
                    trust_remote_code=True,
                    attn_implementation=candidate,
                    dtype=self.dtype,
                    device_map={"": device},
                )
                self.model.config._attn_implementation = candidate
                break
            except Exception as exc:
                last_error = exc
                logger.warning(f"[dit-from-disk] load with {candidate} failed: {exc}")
        if self.model is None:
            raise RuntimeError(f"dit-from-disk: failed to load DiT: {last_error}") from last_error
        # device_map placement can leave stragglers (buffers, modules a
        # custom architecture creates post-hoc) in float32; the fused
        # loader force-casts after construction, so mirror it.
        self.model = self.model.to(self.dtype)
        self.config = self.model.config
        self._sync_alignment_config()
        self._apply_cuda_bool_argsort_workaround()
        self.model.eval()

    def _iar_evict_dit(self) -> None:
        """Drop the DiT entirely; the next model context reloads from disk."""
        if not getattr(self, "offload_dit_to_disk", False):
            return
        if getattr(self, "model", None) is None:
            return
        logger.info("[dit-from-disk] evicting DiT (weights reload from disk on next use)")
        self.model = None
        self._release_system_memory()
`,
			},
			{
				file: "acestep/core/generation/handler/init_service_offload_context.py",
				find: `        model = getattr(self, model_name, None)
        if model is None:
            yield
            return
`,
				replace: `        # iar-patch: dit-from-disk. The DiT is materialized from disk on
        # first use and stays on the GPU across a render batch; eviction
        # happens when planning work starts or the process exits, never
        # by parking weights in system memory.
        if model_name == "model" and getattr(self, "offload_dit_to_disk", False):
            if getattr(self, "model", None) is None:
                rss_before = self._get_rss_mb()
                start_time = time.time()
                self._iar_materialize_dit(self.device)
                load_time = time.time() - start_time
                self.current_offload_cost += load_time
                logger.info(
                    f"[_load_model_context] Loaded model from disk to {self.device} in {load_time:.4f}s "
                    f"(RSS: {rss_before:.0f} -> {self._get_rss_mb():.0f} MB)"
                )
            if hasattr(self, "silence_latent"):
                self.silence_latent = self.silence_latent.to(self.device).to(self.dtype)
            yield
            return

        model = getattr(self, model_name, None)
        if model is None:
            yield
            return
`,
			},
			{
				file: "acestep/core/generation/handler/generate_music_request.py",
				find: `        if self.model is None or self.vae is None or self.text_tokenizer is None or self.text_encoder is None:
`,
				replace: `        # iar-patch: dit-from-disk. A deferred DiT is not "missing": the
        # model context streams it from disk the moment a job needs it.
        model_ok = self.model is not None or getattr(self, "offload_dit_to_disk", False)
        if not model_ok or self.vae is None or self.text_tokenizer is None or self.text_encoder is None:
`,
			},
			{
				file: "acestep/core/generation/handler/generate_music.py",
				find: `        progress = self._resolve_generate_music_progress(progress)
        if self.model is None or self.vae is None or self.text_tokenizer is None or self.text_encoder is None:
            readiness_error = self._validate_generate_music_readiness()
            return readiness_error
`,
				replace: `        progress = self._resolve_generate_music_progress(progress)
        # iar-patch: dit-from-disk. A deferred DiT is not "missing": the
        # model context streams it from disk the moment a job needs it.
        _iar_model_ok = self.model is not None or getattr(self, "offload_dit_to_disk", False)
        if not _iar_model_ok or self.vae is None or self.text_tokenizer is None or self.text_encoder is None:
            readiness_error = self._validate_generate_music_readiness()
            return readiness_error
`,
			},
			{
				file: "acestep/inference.py",
				find: `        # Extract mutable copies of metadata (will be updated by LM if needed)
        bpm = params.bpm
`,
				replace: `        # iar-patch: dit-from-disk. A planning job runs only the LM; drop
        # the DiT first so the whole plan batch runs with the card clear.
        if getattr(params, "plan_only", False) and hasattr(dit_handler, "_iar_evict_dit"):
            dit_handler._iar_evict_dit()

        # Extract mutable copies of metadata (will be updated by LM if needed)
        bpm = params.bpm
`,
			},
		},
	},
}

// ApplyEnginePatches brings the engine checkout under dir up to date
// with every patch, returning the names of those with newly applied
// hunks. A hunk whose full replacement text is already in the file is
// done; otherwise its anchor must match exactly once - anything else
// means the upstream source changed under the patch, which is an
// error, never a silent skip.
func ApplyEnginePatches(dir string) ([]string, error) {
	var applied []string
	for _, p := range enginePatches {
		changed := false
		for i, h := range p.hunks {
			path := filepath.Join(dir, h.file)
			raw, err := os.ReadFile(path)
			if err != nil {
				return applied, fmt.Errorf("patch %s: %w", p.name, err)
			}
			src := string(raw)
			if strings.Contains(src, h.replace) {
				continue
			}
			if n := strings.Count(src, h.find); n != 1 {
				return applied, fmt.Errorf(
					"patch %s hunk %d: anchor matches %d times in %s (engine source changed; update the patch)",
					p.name, i+1, n, h.file)
			}
			src = strings.Replace(src, h.find, h.replace, 1)
			info, err := os.Stat(path)
			if err != nil {
				return applied, fmt.Errorf("patch %s: %w", p.name, err)
			}
			// Write-then-rename: a crash mid-write must not leave a
			// truncated source file that later runs would skip.
			tmp := path + ".iar-patch-tmp"
			if err := os.WriteFile(tmp, []byte(src), info.Mode().Perm()); err != nil {
				return applied, fmt.Errorf("patch %s: %w", p.name, err)
			}
			if err := os.Rename(tmp, path); err != nil {
				os.Remove(tmp)
				return applied, fmt.Errorf("patch %s: %w", p.name, err)
			}
			changed = true
		}
		if changed {
			applied = append(applied, p.name)
		}
	}
	return applied, nil
}
