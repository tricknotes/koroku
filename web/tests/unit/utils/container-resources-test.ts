import { module, test } from 'qunit';
import {
  containerGroupRows,
  containerTotals,
} from 'koroku-console/utils/container-resources';
import type { ContainerUsage } from 'koroku-console/utils/types';

const MB = 1024 ** 2;
const GB = 1024 ** 3;

function container(overrides: Partial<ContainerUsage>): ContainerUsage {
  return {
    name: 'koroku-blog-web-1',
    project: 'koroku-blog',
    service: 'web',
    memory_bytes: 100 * MB,
    memory_limit_bytes: 512 * MB,
    cpu_percent: 1,
    ...overrides,
  };
}

module('Unit | Utility | container-resources', function () {
  test('rows name containers by service within an app, by name elsewhere', function (assert) {
    const [blog, other] = containerGroupRows({
      cpu_count: 4,
      memory_total_bytes: 4 * GB,
      groups: [
        {
          app: 'blog',
          memory_bytes: 1 * GB,
          memory_limit_bytes: 2 * GB,
          cpu_percent: 25,
          containers: [
            container({ memory_bytes: 470 * MB }),
            container({
              name: 'koroku-blog-db-1',
              service: 'db',
              memory_bytes: 554 * MB,
              memory_limit_bytes: 1.5 * GB,
            }),
          ],
        },
        {
          app: null,
          memory_bytes: 10 * MB,
          memory_limit_bytes: null,
          cpu_percent: 0,
          containers: [
            container({
              name: 'registry',
              project: '',
              service: '',
              memory_bytes: 10 * MB,
              memory_limit_bytes: null,
            }),
          ],
        },
      ],
    });

    assert.strictEqual(blog?.label, 'blog');
    assert.strictEqual(blog?.memory, '1.0 GB');
    assert.strictEqual(blog?.memoryPercent, 25, 'a share of the host');
    assert.strictEqual(blog?.limit, '上限 2.0 GB（50%）');
    assert.strictEqual(
      blog?.nearLimit,
      'web',
      'judged against each container’s own limit, not the total',
    );
    assert.strictEqual(blog?.breakdown, 'web 470.0 MB、db 554.0 MB');
    assert.strictEqual(blog?.cpu, '25%');

    assert.strictEqual(other?.app, null);
    assert.strictEqual(other?.label, 'その他');
    assert.strictEqual(other?.limit, '上限なし');
    assert.strictEqual(other?.nearLimit, '');
    assert.strictEqual(other?.breakdown, 'registry 10.0 MB');
  });

  test('totals add every group up', function (assert) {
    assert.deepEqual(
      containerTotals({
        cpu_count: 2,
        memory_total_bytes: 4 * GB,
        groups: [
          {
            app: 'blog',
            memory_bytes: 1 * GB,
            memory_limit_bytes: null,
            cpu_percent: 3.2,
            containers: [],
          },
          {
            app: null,
            memory_bytes: 1 * GB,
            memory_limit_bytes: null,
            cpu_percent: 0.1,
            containers: [],
          },
        ],
      }),
      {
        memory: '2.0 GB',
        memoryPercent: '50%',
        cpu: '3.3%',
        hostMemory: '4.0 GB',
        cpuCount: 2,
      },
    );
  });
});
