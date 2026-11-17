import { FormEvent, useCallback, useEffect, useState } from "react";

type Step = { label: string; state: "pending" | "done" };

type Status = "draft" | "running" | "success" | "failed";

type Job = {
  id: string;
  title: string;
  address: string;
  status: Status;
  error: string;
  runId: string;
  filename: string;
  checklist: Step[];
  createdAt: string;
};

type Account = { id: string; email: string };

// How often the open sheet is refreshed while the worker runs it.
const POLL_MS = 1000;

class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "include", ...init });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    // Not JSON (a proxy error page, say): fall back to the status text below.
  }
  if (!res.ok) {
    const body = data as { error?: unknown } | null;
    const message = body && typeof body.error === "string" ? body.error : res.statusText || "Request failed";
    throw new ApiError(message, res.status);
  }
  return data as T;
}

function messageOf(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback;
}

function uploadBrochure(id: string, file: File): Promise<Job> {
  const body = new FormData();
  body.append("file", file);
  return api<Job>(`/api/jobs/${id}/file`, { method: "POST", body });
}

export function App() {
  const [me, setMe] = useState<Account | null>(null);
  const [ready, setReady] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [error, setError] = useState("");

  // The list is the one copy of every sheet; the open sheet is looked up in
  // it, so a poll that refreshes a sheet updates its row and its page together.
  const selected = jobs.find((job) => job.id === selectedId) ?? null;

  const upsert = useCallback((job: Job) => {
    setJobs((current) =>
      current.some((item) => item.id === job.id)
        ? current.map((item) => (item.id === job.id ? job : item))
        : [job, ...current],
    );
  }, []);

  const signOut = useCallback(() => {
    setMe(null);
    setJobs([]);
    setSelectedId(null);
  }, []);

  useEffect(() => {
    api<Account>("/api/me")
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  useEffect(() => {
    if (!me) {
      return;
    }
    api<Job[]>("/api/jobs")
      .then(setJobs)
      .catch((err) => setError(messageOf(err, "Could not load sheets")));
  }, [me]);

  // An account runs at most one sheet at a time. While it does, refresh that
  // sheet every second, so the checklist moves without a reload. The chain
  // stops by itself once the sheet reaches success or failed.
  const runningId = jobs.find((job) => job.status === "running")?.id;
  useEffect(() => {
    if (!runningId) {
      return;
    }
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      try {
        const job = await api<Job>(`/api/jobs/${runningId}`);
        if (!stopped) {
          upsert(job);
        }
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          signOut();
          return;
        }
        // Anything else is likely transient: keep polling.
      }
      if (!stopped) {
        timer = setTimeout(tick, POLL_MS);
      }
    };
    timer = setTimeout(tick, POLL_MS);
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [runningId, upsert, signOut]);

  if (!ready) {
    return <p className="muted">Loading…</p>;
  }
  if (!me) {
    return <Auth onIn={setMe} />;
  }

  return (
    <main>
      <header>
        <div>
          <p className="eyebrow">Real estate</p>
          <h1>Listing board</h1>
          <p className="muted">{me.email}</p>
        </div>
        <button
          className="ghost"
          onClick={() => {
            api("/api/logout", { method: "POST" }).finally(signOut);
          }}
        >
          Log out
        </button>
      </header>
      {error ? (
        <p className="error" role="alert">
          {error}
        </p>
      ) : null}
      <Create
        onSaved={(job) => {
          upsert(job);
          setSelectedId(job.id);
        }}
        onError={setError}
      />
      <section className="layout">
        <ul className="list">
          {jobs.length === 0 ? <li className="muted">No sheets yet.</li> : null}
          {jobs.map((job) => (
            <li key={job.id}>
              <button className={selectedId === job.id ? "row on" : "row"} onClick={() => setSelectedId(job.id)}>
                <span>
                  <strong>{job.title}</strong>
                  <span className="muted">{job.address}</span>
                </span>
                <span className={"status " + job.status}>{job.status}</span>
              </button>
            </li>
          ))}
        </ul>
        {selected ? (
          <Sheet job={selected} onChange={upsert} onError={setError} />
        ) : (
          <p className="muted">Select a sheet.</p>
        )}
      </section>
    </main>
  );
}

function Auth({ onIn }: { onIn: (account: Account) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      const account = await api<Account>("/api/" + mode, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      onIn(account);
    } catch (err) {
      setError(messageOf(err, "Could not sign in"));
    }
  }

  return (
    <main className="narrow">
      <p className="eyebrow">Real estate</p>
      <h1>Listing board</h1>
      <form onSubmit={submit}>
        <label>
          Email
          <input value={email} onChange={(event) => setEmail(event.target.value)} type="email" required />
        </label>
        <label>
          Password
          <input
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            type="password"
            minLength={mode === "register" ? 8 : undefined}
            required
          />
        </label>
        {error ? (
          <p className="error" role="alert">
            {error}
          </p>
        ) : null}
        <button type="submit">{mode === "login" ? "Sign in" : "Create account"}</button>
      </form>
      <button className="ghost" onClick={() => setMode(mode === "login" ? "register" : "login")}>
        {mode === "login" ? "Need an account?" : "Already have an account?"}
      </button>
    </main>
  );
}

function Create({ onSaved, onError }: { onSaved: (job: Job) => void; onError: (message: string) => void }) {
  const [title, setTitle] = useState("");
  const [address, setAddress] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  // Changing the key clears the file input, which React can't set by value.
  const [fileKey, setFileKey] = useState(0);

  async function submit(event: FormEvent) {
    event.preventDefault();
    onError("");
    setBusy(true);
    try {
      const job = await api<Job>("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, address }),
      });
      // Show the sheet straight away: if the upload is rejected, the sheet
      // still exists and its page offers to attach another file.
      onSaved(job);
      setTitle("");
      setAddress("");
      setFile(null);
      setFileKey((key) => key + 1);
      if (file) {
        onSaved(await uploadBrochure(job.id, file));
      }
    } catch (err) {
      onError(messageOf(err, "Could not create the sheet"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="create" onSubmit={submit}>
      <label>
        Title
        <input value={title} onChange={(event) => setTitle(event.target.value)} required />
      </label>
      <label>
        Address
        <input value={address} onChange={(event) => setAddress(event.target.value)} required />
      </label>
      <label>
        Brochure (PDF or text, up to 1 MiB)
        <input
          key={fileKey}
          type="file"
          accept=".pdf,.txt,application/pdf,text/plain"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
        />
      </label>
      <button type="submit" disabled={busy}>
        {busy ? "Creating…" : "Create sheet"}
      </button>
    </form>
  );
}

function Sheet({
  job,
  onChange,
  onError,
}: {
  job: Job;
  onChange: (job: Job) => void;
  onError: (message: string) => void;
}) {
  const [busy, setBusy] = useState(false);

  async function run(action: () => Promise<Job>, fallback: string) {
    onError("");
    setBusy(true);
    try {
      onChange(await action());
    } catch (err) {
      onError(messageOf(err, fallback));
    } finally {
      setBusy(false);
    }
  }

  const done = job.checklist.filter((step) => step.state === "done").length;

  return (
    <article className="sheet">
      <h2>{job.title}</h2>
      <p>{job.address}</p>
      {/* aria-live announces progress to screen readers as the worker moves. */}
      <div aria-live="polite">
        <p className={"status " + job.status}>
          {job.status}
          {job.status === "running" ? ` · step ${Math.min(done + 1, job.checklist.length)} of ${job.checklist.length}` : null}
        </p>
        {job.status === "failed" && job.error ? (
          <p className="error" role="alert">
            {job.error}
          </p>
        ) : null}
        <ol className="checklist">
          {job.checklist.map((step) => (
            <li key={step.label} className={step.state}>
              {step.label}
              <span className="sr-only"> ({step.state})</span>
            </li>
          ))}
        </ol>
      </div>

      {job.filename ? (
        <p className="muted">
          Brochure:{" "}
          <a href={`/api/jobs/${job.id}/file`} download>
            {job.filename}
          </a>
        </p>
      ) : (
        <Attach
          disabled={busy}
          onFile={(file) => run(() => uploadBrochure(job.id, file), "Could not attach the brochure")}
        />
      )}

      {job.status === "draft" ? (
        <div className="actions">
          <button
            disabled={busy || !job.filename}
            onClick={() => run(() => api<Job>(`/api/jobs/${job.id}/start`, { method: "POST" }), "Could not start the sheet")}
          >
            Start
          </button>
          {!job.filename ? <span className="muted">Attach a brochure to start.</span> : null}
        </div>
      ) : null}
      {job.runId ? <p className="muted run">Run {job.runId}</p> : null}
    </article>
  );
}

function Attach({ disabled, onFile }: { disabled: boolean; onFile: (file: File) => void }) {
  const [file, setFile] = useState<File | null>(null);

  return (
    <form
      className="attach"
      onSubmit={(event) => {
        event.preventDefault();
        if (file) {
          onFile(file);
        }
      }}
    >
      <label>
        Brochure (PDF or text, up to 1 MiB)
        <input
          type="file"
          accept=".pdf,.txt,application/pdf,text/plain"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          required
        />
      </label>
      <button type="submit" disabled={disabled || !file}>
        Attach brochure
      </button>
    </form>
  );
}
