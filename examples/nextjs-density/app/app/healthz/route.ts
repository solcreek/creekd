// Simple liveness endpoint. The bench harness waits for this to
// return 200 before timing "spawn-to-ready" or sampling RSS.
export const dynamic = "force-dynamic";

export function GET() {
  return new Response("ok\n", {
    status: 200,
    headers: { "content-type": "text/plain" },
  });
}
