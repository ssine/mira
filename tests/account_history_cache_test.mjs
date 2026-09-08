import assert from 'node:assert/strict';
import { AccountHistory } from '../server/public/account-history.js';
const chart = Object.assign(Object.create(AccountHistory.prototype), {
 cache:new Map(), range:'7d', render(){}, node:{nodeId:'fixture'}, account:{type:'chatgpt',email:'same@example.test'},
});
let replies=[];
globalThis.fetch = () => new Promise(resolve => replies.push(resolve));
const response = points => ({ok:true,json:async()=>({points})});
chart.select(chart.node, chart.account, true);
const settle = () => new Promise(resolve => setImmediate(resolve));
replies.shift()(response([])); await settle();
assert.ok(chart.cache.get(chart.key).expiresAt-Date.now()<=30_000,'empty history has short TTL');
chart.select(chart.node,chart.account,true);
assert.equal(replies.length,0,'ordinary refresh reuses cache');
chart.select(chart.node,chart.account,true,true);
assert.equal(replies.length,1,'explicit range selection retries cached empty history');
replies.shift()(response([{remaining:80}])); await settle();
assert.equal(chart.data.points[0].remaining,80);
// Abort followed by the same key must not let an old response replace new data.
chart.select(chart.node,chart.account,true,true);
const stale=replies.shift();
chart.range='24h'; chart.select(chart.node,chart.account,true);
const middle=replies.shift();
chart.range='7d'; chart.select(chart.node,chart.account,true,true);
const latest=replies.shift();
latest(response([{remaining:60}])); await settle();
stale(response([])); middle(response([])); await settle();
assert.equal(chart.data.points[0].remaining,60,'aborted same-key request cannot overwrite latest result');
console.log('account history cache tests passed');
