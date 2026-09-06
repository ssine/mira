// Public Standard USD prices per million tokens, verified 2026-09-06.
// This is an API-equivalent estimate, not a ChatGPT charge or historical invoice.
export const pricingDate = "2026-09-06";
export const pricingSource = "https://developers.openai.com/api/docs/pricing";
export const modelPrices = Object.freeze({
  "gpt-6-astra": { input: 10, cached: 1, write: 12.5, output: 50, longContext: 272000 },
  "gpt-5.6-sol": { input: 4, cached: 0.4, write: 5, output: 20, longContext: 272000 },
  "gpt-5.6-terra": { input: 2, cached: 0.2, write: 2.5, output: 12, longContext: 272000 },
  "gpt-5.6-luna": { input: 0.2, cached: 0.02, write: 0.25, output: 1.2, longContext: 272000 },
  "gpt-5.5": { input: 5, cached: 0.5, output: 30, longContext: 272000 },
  "gpt-5.4": { input: 2.5, cached: 0.25, output: 15, longContext: 272000 },
  "gpt-5.3-codex": { input: 1.75, cached: 0.175, output: 14 },
});

export function usageCounts(value) {
  const counts = [value?.input_tokens, value?.cached_input_tokens, value?.cache_write_input_tokens ?? 0, value?.output_tokens];
  return counts.every(count => Number.isSafeInteger(count) && count >= 0) && counts[1] + counts[2] <= counts[0] ? counts : null;
}

export function priceUsage(model, counts) {
  const rate = Object.hasOwn(modelPrices, model ?? "") ? modelPrices[model] : null;
  if (!rate || counts[2] && rate.write == null) return null;
  const [input, cached, write, output] = counts;
  const long = Boolean(rate.longContext && input > rate.longContext);
  // Integer nanodollars avoid per-request rounding loss for small/cheap calls.
  const nano = (tokens, price, multiplier) => BigInt(tokens) * BigInt(Math.round(price * multiplier * 1000));
  return {
    input: nano(input - cached - write, rate.input, long ? 2 : 1),
    cached: nano(cached, rate.cached, long ? 2 : 1),
    write: nano(write, rate.write ?? 0, long ? 2 : 1),
    output: nano(output, rate.output, long ? 1.5 : 1), long,
  };
}
