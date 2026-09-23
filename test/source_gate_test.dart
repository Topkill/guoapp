import 'package:duanju_app/app_build.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/main.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'fixtures.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  Future<LocalStore> create([Map<String, Object> initial = const {}]) async {
    SharedPreferences.setMockInitialValues(Map.of(initial));
    final store = LocalStore(await SharedPreferences.getInstance());
    addTearDown(store.dispose);
    return store;
  }

  test('default visibility hides restricted sources until unlocked', () async {
    final store = await create();
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
    expect(
      store.sources.map((site) => site.id),
      allSourcesEnabled ? ['hongguo', 'sorani'] : ['hongguo'],
    );
    expect(store.allowsSource('sorani'), allSourcesEnabled);
    expect(store.allowsSource('huangdou'), allSourcesEnabled);
  });

  test('enabling the gate hides restricted sources and unlocks once', () async {
    final store = await create();
    await store.enableSourceGate('666');
    expect(store.sourceGateEnabled, isTrue);
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);

    store.lockSources();
    expect(store.sourcesUnlocked, isFalse);
    expect(store.sources.length, allSourcesEnabled ? 2 : 1);
    expect(store.allowsSource('huangdou'), isFalse);
    expect(store.allowsSource('hongguo'), isTrue);
    if (allSourcesEnabled) expect(store.allowsSource('sorani'), isTrue);

    await expectLater(store.unlockSources('777'), throwsStateError);
    expect(store.sourcesUnlocked, isFalse);
    await store.unlockSources('666');
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);
  });

  test('a saved gate hides sources again after restart', () async {
    final store = await create();
    await store.enableSourceGate('666');
    store.lockSources();
    final restarted = LocalStore(store.preferences);
    addTearDown(restarted.dispose);
    expect(restarted.sourceGateEnabled, isTrue);
    expect(restarted.sourcesUnlocked, isFalse);
    expect(restarted.sources.length, allSourcesEnabled ? 2 : 1);
    await restarted.unlockSources('666');
    expect(restarted.sourcesUnlocked, isTrue);
  });

  test('disabling the gate restores every compiled source', () async {
    final store = await create();
    await store.enableSourceGate('666');
    store.lockSources();
    await store.disableSourceGate();
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);
  });

  test('an invalid pin must be 4 to 12 digits', () async {
    final store = await create();
    await expectLater(store.enableSourceGate('12'), throwsStateError);
    await expectLater(store.enableSourceGate('abcdef'), throwsStateError);
    await expectLater(
      store.enableSourceGate('1234567890123'),
      throwsStateError,
    );
    expect(store.sourceGateEnabled, isFalse);
  });

  test('a damaged gate record falls back to visible sources', () async {
    final store = await create({
      'sourceGateEnabled': true,
      'sourceGateSalt': 'bad',
      'sourceGateHash': 'bad',
    });
    expect(store.configurationError, isNull);
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
  });

  testWidgets('tapping 最近观看 six times opens the gate dialog', (tester) async {
    final store = await create();
    await store.enableSourceGate('666');
    store.lockSources();
    await tester.pumpWidget(
      DuanjuApp(repository: FixtureRepository(), store: store),
    );
    await tester.pumpAndSettle();
    final recent = find.text('最近观看');
    expect(recent, findsWidgets);
    for (var i = 0; i < 6; i++) {
      await tester.tap(recent.last, warnIfMissed: false);
      await tester.pump();
    }
    await tester.pumpAndSettle();
    expect(find.text('站源密码锁'), findsOneWidget);
    expect(find.text('解锁'), findsOneWidget);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(find.text('站源密码锁'), findsNothing);
  });
}
