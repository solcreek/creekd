// Server Component (default in App Router). Reads APP_NAME from
// process.env so each bench instance returns its own identity —
// helpful when checking that dispatch is routing to the right
// supervised process under N parallel apps.
export default function Page() {
  const name = process.env.APP_NAME ?? "anonymous";
  return (
    <main style={{ fontFamily: "system-ui", padding: "2rem" }}>
      <h1>creekd density bench</h1>
      <p>
        Hello from <strong>{name}</strong>. This Next.js app is supervised by
        creekd. Each instance is its own OS process; cgroup limits enforce a
        per-app memory cap.
      </p>
      <p>
        See <a href="/healthz">/healthz</a> for the liveness endpoint.
      </p>
    </main>
  );
}
