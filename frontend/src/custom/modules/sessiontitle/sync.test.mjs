import test from 'node:test'
import assert from 'node:assert/strict'
import { synchronizeTitle } from './sync.ts'

test('late title is published once without a second model generation', async () => {
  let reads = 0, repairs = 0
  const published = []
  const options = { key: 'late', delays: [0, 0, 0], authorized: () => true,
    read: async () => ({ session_id: 'late', title: ++reads > 1 ? 'Saved title' : '' }),
    repair: async () => { repairs++; return { session_id: 'late', title: 'Unexpected' } },
    publish: value => published.push(value),
  }
  await Promise.all([synchronizeTitle(options), synchronizeTitle(options)])
  assert.equal(reads, 2)
  assert.equal(repairs, 0)
  assert.deepEqual(published, [{ session_id: 'late', title: 'Saved title' }])
})

test('missing title is recovered only after the bounded read attempts', async () => {
  let reads = 0, repairs = 0
  const published = []
  await synchronizeTitle({ key: 'missing', delays: [0, 0], authorized: () => true,
    read: async () => { reads++; return { session_id: 'missing', title: '' } },
    repair: async () => { repairs++; return { session_id: 'missing', title: 'Recovered' } },
    publish: value => published.push(value),
  })
  assert.equal(reads, 2)
  assert.equal(repairs, 1)
  assert.equal(published[0].title, 'Recovered')
})

test('overlapping history and stream subscriptions both receive the shared result', async () => {
  let reads = 0
  const received = []
  const base = { key: 'subscribers', delays: [0], authorized: () => true,
    read: async () => { reads++; return { session_id: 'subscribers', title: 'Shared' } },
    repair: async () => assert.fail('must not repair a saved title'),
  }
  await Promise.all([
    synchronizeTitle({ ...base, publish: value => received.push(['history', value.title]) }),
    synchronizeTitle({ ...base, publish: value => received.push(['stream', value.title]) }),
  ])
  assert.equal(reads, 1)
  assert.deepEqual(received, [['history', 'Shared'], ['stream', 'Shared']])
})

test('session removal or logout stops title recovery', async () => {
  for (const status of [401, 403, 404]) {
    await synchronizeTitle({ key: `removed-${status}`, delays: [0], authorized: () => true,
      read: async () => { throw { status } },
      repair: async () => { assert.fail('must not repair an inaccessible session') },
      publish: () => assert.fail('must not publish an inaccessible session'),
    })
  }
  await synchronizeTitle({ key: 'logout', delays: [0], authorized: () => false,
    read: async () => assert.fail('must not read after logout'),
    repair: async () => assert.fail('must not repair after logout'),
    publish: () => assert.fail('must not publish after logout'),
  })
})
