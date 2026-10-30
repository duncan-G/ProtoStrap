using System.Diagnostics;
using System.Runtime.InteropServices;

namespace Sample.Shared;

/// <summary>Cpu holds busy loops, so that work shows as CPU, not waiting.</summary>
public static class Cpu
{
    private static double sink;

    /// <summary>
    /// BurnWallTime spins for <paramref name="ms"/> of wall-clock time, however much CPU that
    /// gets, so a job always takes the same time.
    /// </summary>
    public static TimeSpan BurnWallTime(int ms)
    {
        var watch = Stopwatch.StartNew();
        while (watch.ElapsedMilliseconds < ms)
            Work();
        return watch.Elapsed;
    }

    /// <summary>
    /// BurnCpuTime spins until this thread has used <paramref name="ms"/> of CPU time, so a
    /// request costs the same CPU however busy the replica is, and takes longer when CPU is
    /// short. Where thread CPU time can't be read (not Linux), it spins for wall-clock time
    /// instead.
    /// </summary>
    /// <returns>It returns the wall-clock time the spinning took.</returns>
    public static TimeSpan BurnCpuTime(int ms)
    {
        if (!ThreadCpuTime.Available)
            return BurnWallTime(ms);
        var watch = Stopwatch.StartNew();
        var until = ThreadCpuTime.Now() + TimeSpan.FromMilliseconds(ms);
        while (ThreadCpuTime.Now() < until)
            Work();
        return watch.Elapsed;
    }

    private static void Work()
    {
        var x = Volatile.Read(ref sink);
        for (var i = 0; i < 10_000; i++)
            x = Math.Sqrt(x + i);
        Volatile.Write(ref sink, x); // This write keeps the loop from being optimised away.
    }

    private static class ThreadCpuTime
    {
        private const int ClockThreadCpuTimeId = 3; // This is Linux's CLOCK_THREAD_CPUTIME_ID.

        public static readonly bool Available = OperatingSystem.IsLinux() && CanRead();

        public static TimeSpan Now()
        {
            ClockGetTime(ClockThreadCpuTimeId, out var time);
            return TimeSpan.FromTicks(time.Seconds * TimeSpan.TicksPerSecond + time.Nanoseconds / 100);
        }

        private static bool CanRead()
        {
            try
            {
                return ClockGetTime(ClockThreadCpuTimeId, out _) == 0;
            }
            catch (Exception ex) when (ex is DllNotFoundException or EntryPointNotFoundException)
            {
                return false;
            }
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct Timespec
        {
            public long Seconds;
            public long Nanoseconds;
        }

        [DllImport("libc", EntryPoint = "clock_gettime")]
        private static extern int ClockGetTime(int clockId, out Timespec time);
    }
}
